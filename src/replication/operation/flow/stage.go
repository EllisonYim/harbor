// Copyright Project Harbor Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package flow

import (
	"errors"
	"fmt"
	"github.com/goharbor/harbor/src/replication/filter"
	"time"

	"github.com/goharbor/harbor/src/lib/log"
	adp "github.com/goharbor/harbor/src/replication/adapter"
	"github.com/goharbor/harbor/src/replication/dao/models"
	"github.com/goharbor/harbor/src/replication/model"
	"github.com/goharbor/harbor/src/replication/operation/execution"
	"github.com/goharbor/harbor/src/replication/operation/scheduler"
	"github.com/goharbor/harbor/src/replication/util"
)

// get/create the source registry, destination registry, source adapter and destination adapter
func initialize(policy *model.Policy) (adp.Adapter, adp.Adapter, error) {
	var srcAdapter, dstAdapter adp.Adapter
	var err error

	// create the source registry adapter
	srcFactory, err := adp.GetFactory(policy.SrcRegistry.Type)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get adapter factory for registry type %s: %v", policy.SrcRegistry.Type, err)
	}
	srcAdapter, err = srcFactory.Create(policy.SrcRegistry)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create adapter for source registry %s: %v", policy.SrcRegistry.URL, err)
	}

	// create the destination registry adapter
	dstFactory, err := adp.GetFactory(policy.DestRegistry.Type)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get adapter factory for registry type %s: %v", policy.DestRegistry.Type, err)
	}
	dstAdapter, err = dstFactory.Create(policy.DestRegistry)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create adapter for destination registry %s: %v", policy.DestRegistry.URL, err)
	}
	log.Debug("replication flow initialization completed")
	return srcAdapter, dstAdapter, nil
}

// fetch resources from the source registry
func fetchResources(adapter adp.Adapter, policy *model.Policy) ([]*model.Resource, error) {
	var resTypes []model.ResourceType
	for _, filter := range policy.Filters {
		if filter.Type == model.FilterTypeResource {
			resTypes = append(resTypes, filter.Value.(model.ResourceType))
		}
	}
	if len(resTypes) == 0 {
		info, err := adapter.Info()
		if err != nil {
			return nil, fmt.Errorf("failed to get the adapter info: %v", err)
		}
		resTypes = append(resTypes, info.SupportedResourceTypes...)
	}

	fetchArtifact := false
	fetchChart := false
	for _, resType := range resTypes {
		if resType == model.ResourceTypeChart {
			fetchChart = true
			continue
		}
		fetchArtifact = true
	}

	var resources []*model.Resource
	// artifacts
	if fetchArtifact {
		reg, ok := adapter.(adp.ArtifactRegistry)
		if !ok {
			return nil, fmt.Errorf("the adapter doesn't implement the ArtifactRegistry interface")
		}
		res, err := reg.FetchArtifacts(policy.Filters)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch artifacts: %v", err)
		}
		resources = append(resources, res...)
		log.Debug("fetch artifacts completed")
	}
	// charts
	if fetchChart {
		reg, ok := adapter.(adp.ChartRegistry)
		if !ok {
			return nil, fmt.Errorf("the adapter doesn't implement the ChartRegistry interface")
		}
		res, err := reg.FetchCharts(policy.Filters)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch charts: %v", err)
		}
		resources = append(resources, res...)
		log.Debug("fetch charts completed")
	}

	log.Debug("fetch resources from the source registry completed")
	return resources, nil
}

// apply the filters to the resources and returns the filtered resources
func filterResources(resources []*model.Resource, filters []*model.Filter) ([]*model.Resource, error) {
	resources, err := filter.DoFilterResources(resources, filters)
	if err != nil {
		return nil, err
	}
	log.Debug("filter resources completed")
	return resources, nil
}

// assemble the source resources by filling the registry information
func assembleSourceResources(resources []*model.Resource,
	policy *model.Policy) []*model.Resource {
	for _, resource := range resources {
		resource.Registry = policy.SrcRegistry
	}
	log.Debug("assemble the source resources completed")
	return resources
}

// assemble the destination resources by filling the metadata, registry and override properties
func assembleDestinationResources(resources []*model.Resource,
	policy *model.Policy) []*model.Resource {
	var result []*model.Resource
	for _, resource := range resources {
		res := &model.Resource{
			Type:         resource.Type,
			Registry:     policy.DestRegistry,
			ExtendedInfo: resource.ExtendedInfo,
			Deleted:      resource.Deleted,
			IsDeleteTag:  resource.IsDeleteTag,
			Override:     policy.Override,
		}
		res.Metadata = &model.ResourceMetadata{
			Repository: &model.Repository{
				Name:     replaceNamespace(resource.Metadata.Repository.Name, policy.DestNamespace),
				Metadata: resource.Metadata.Repository.Metadata,
			},
			Vtags:     resource.Metadata.Vtags,
			Artifacts: resource.Metadata.Artifacts,
		}
		result = append(result, res)
	}
	log.Debug("assemble the destination resources completed")
	return result
}

// do the prepare work for pushing/uploading the resources: create the namespace or repository
func prepareForPush(adapter adp.Adapter, resources []*model.Resource) error {
	if err := adapter.PrepareForPush(resources); err != nil {
		return fmt.Errorf("failed to do the prepare work for pushing/uploading resources: %v", err)
	}
	log.Debug("the prepare work for pushing/uploading resources completed")
	return nil
}

// preprocess
func preprocess(scheduler scheduler.Scheduler, srcResources, dstResources []*model.Resource) ([]*scheduler.ScheduleItem, error) {
	items, err := scheduler.Preprocess(srcResources, dstResources)
	if err != nil {
		return nil, fmt.Errorf("failed to preprocess the resources: %v", err)
	}
	log.Debug("preprocess the resources completed")
	return items, nil
}

// create task records in database
func createTasks(mgr execution.Manager, executionID int64, items []*scheduler.ScheduleItem) error {
	for _, item := range items {
		operation := "copy"
		if item.DstResource.Deleted {
			operation = "deletion"
			if item.DstResource.IsDeleteTag {
				operation = "tag deletion"
			}
		}

		task := &models.Task{
			ExecutionID:  executionID,
			Status:       models.TaskStatusInitialized,
			ResourceType: string(item.SrcResource.Type),
			SrcResource:  getResourceName(item.SrcResource),
			DstResource:  getResourceName(item.DstResource),
			Operation:    operation,
		}

		id, err := mgr.CreateTask(task)
		if err != nil {
			// if failed to create the task for one of the items,
			// the whole execution is marked as failure and all
			// the items will not be submitted
			return fmt.Errorf("failed to create task records for the execution %d: %v", executionID, err)
		}

		item.TaskID = id
		log.Debugf("task record %d for the execution %d created", id, executionID)
	}
	return nil
}

// schedule the replication tasks and update the task's status
// returns the count of tasks which have been scheduled and the error
func schedule(scheduler scheduler.Scheduler, executionMgr execution.Manager, items []*scheduler.ScheduleItem) (int, error) {
	results, err := scheduler.Schedule(items)
	if err != nil {
		return 0, fmt.Errorf("failed to schedule the tasks: %v", err)
	}

	allFailed := true
	n := len(results)
	for _, result := range results {
		// if the task is failed to be submitted, update the status of the
		// task as failure
		now := time.Now()
		if result.Error != nil {
			log.Errorf("failed to schedule the task %d: %v", result.TaskID, result.Error)
			if err = executionMgr.UpdateTask(&models.Task{
				ID:      result.TaskID,
				Status:  models.TaskStatusFailed,
				EndTime: now,
			}, "Status", "EndTime"); err != nil {
				log.Errorf("failed to update the task status %d: %v", result.TaskID, err)
			}
			continue
		}
		allFailed = false
		// if the task is submitted successfully, update the status, job ID and start time
		if err = executionMgr.UpdateTaskStatus(result.TaskID, models.TaskStatusPending, 0, models.TaskStatusInitialized); err != nil {
			log.Errorf("failed to update the task status %d: %v", result.TaskID, err)
		}
		if err = executionMgr.UpdateTask(&models.Task{
			ID:        result.TaskID,
			JobID:     result.JobID,
			StartTime: now,
		}, "JobID", "StartTime"); err != nil {
			log.Errorf("failed to update the task %d: %v", result.TaskID, err)
		}
		log.Debugf("the task %d scheduled", result.TaskID)
	}
	// if all the tasks are failed, return err
	if allFailed {
		return n, errors.New("all tasks are failed")
	}
	return n, nil
}

// check whether the execution is stopped
func isExecutionStopped(mgr execution.Manager, id int64) (bool, error) {
	execution, err := mgr.Get(id)
	if err != nil {
		return false, err
	}
	if execution == nil {
		return false, fmt.Errorf("execution %d not found", id)
	}
	return execution.Status == models.ExecutionStatusStopped, nil
}

// return the name with format "res_name" or "res_name:[vtag1,vtag2,vtag3]"
// if the resource has vtags
func getResourceName(res *model.Resource) string {
	if res == nil {
		return ""
	}
	meta := res.Metadata
	if meta == nil {
		return ""
	}
	n := 0
	if len(meta.Artifacts) > 0 {
		for _, artifact := range meta.Artifacts {
			// contains tags
			if len(artifact.Tags) > 0 {
				n += len(artifact.Tags)
				continue
			}
			// contains no tag, count digest
			if len(artifact.Digest) > 0 {
				n++
			}
		}
	} else {
		n = len(meta.Vtags)
	}

	return fmt.Sprintf("%s [%d item(s) in total]", meta.Repository.Name, n)
}

// repository:c namespace:n -> n/c
// repository:b/c namespace:n -> n/c
// repository:a/b/c namespace:n -> n/c
func replaceNamespace(repository string, namespace string) string {
	if len(namespace) == 0 {
		return repository
	}
	_, rest := util.ParseRepository(repository)
	return fmt.Sprintf("%s/%s", namespace, rest)
}
