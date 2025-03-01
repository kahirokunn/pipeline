/*
Copyright 2019 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package resources

import (
	"fmt"
	"sort"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
)

// ResolvedResultRefs represents all of the ResolvedResultRef for a pipeline task
type ResolvedResultRefs []*ResolvedResultRef

// ResolvedResultRef represents a result ref reference that has been fully resolved (value has been populated).
// If the value is from a Result, then the ResultReference will be populated to point to the ResultReference
// which resulted in the value
type ResolvedResultRef struct {
	Value           v1beta1.ResultValue
	ResultReference v1beta1.ResultRef
	FromTaskRun     string
	FromRun         string
}

// ResolveResultRef resolves any ResultReference that are found in the target ResolvedPipelineTask
func ResolveResultRef(pipelineRunState PipelineRunState, target *ResolvedPipelineTask) (ResolvedResultRefs, string, error) {
	resolvedResultRefs, pt, err := convertToResultRefs(pipelineRunState, target)
	if err != nil {
		return nil, pt, err
	}
	return validateArrayResultsIndex(removeDup(resolvedResultRefs))
}

// ResolveResultRefs resolves any ResultReference that are found in the target ResolvedPipelineTask
func ResolveResultRefs(pipelineRunState PipelineRunState, targets PipelineRunState) (ResolvedResultRefs, string, error) {
	var allResolvedResultRefs ResolvedResultRefs
	for _, target := range targets {
		resolvedResultRefs, pt, err := convertToResultRefs(pipelineRunState, target)
		if err != nil {
			return nil, pt, err
		}
		allResolvedResultRefs = append(allResolvedResultRefs, resolvedResultRefs...)
	}
	return validateArrayResultsIndex(removeDup(allResolvedResultRefs))
}

// validateArrayResultsIndex checks if the result array indexing reference is out of bound of the array size
func validateArrayResultsIndex(allResolvedResultRefs ResolvedResultRefs) (ResolvedResultRefs, string, error) {
	for _, r := range allResolvedResultRefs {
		if r.Value.Type == v1beta1.ParamTypeArray {
			if r.ResultReference.ResultsIndex >= len(r.Value.ArrayVal) {
				return nil, "", fmt.Errorf("Array Result Index %d for Task %s Result %s is out of bound of size %d", r.ResultReference.ResultsIndex, r.ResultReference.PipelineTask, r.ResultReference.Result, len(r.Value.ArrayVal))
			}
		}
	}
	return allResolvedResultRefs, "", nil
}

func removeDup(refs ResolvedResultRefs) ResolvedResultRefs {
	if refs == nil {
		return nil
	}
	resolvedResultRefByRef := make(map[v1beta1.ResultRef]*ResolvedResultRef, len(refs))
	for _, resolvedResultRef := range refs {
		resolvedResultRefByRef[resolvedResultRef.ResultReference] = resolvedResultRef
	}
	deduped := make([]*ResolvedResultRef, 0, len(resolvedResultRefByRef))

	// Sort the resulting keys to produce a deterministic ordering.
	order := make([]v1beta1.ResultRef, 0, len(refs))
	for key := range resolvedResultRefByRef {
		order = append(order, key)
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].PipelineTask > order[j].PipelineTask {
			return false
		}
		if order[i].Result > order[j].Result {
			return false
		}
		return true
	})

	for _, key := range order {
		deduped = append(deduped, resolvedResultRefByRef[key])
	}
	return deduped
}

// convertToResultRefs walks a PipelineTask looking for result references. If any are
// found they are resolved to a value by searching pipelineRunState. The list of resolved
// references are returned. If an error is encountered due to an invalid result reference
// then a nil list and error is returned instead.
func convertToResultRefs(pipelineRunState PipelineRunState, target *ResolvedPipelineTask) (ResolvedResultRefs, string, error) {
	var resolvedResultRefs ResolvedResultRefs
	for _, ref := range v1beta1.PipelineTaskResultRefs(target.PipelineTask) {
		resolved, pt, err := resolveResultRef(pipelineRunState, ref)
		if err != nil {
			return nil, pt, err
		}
		resolvedResultRefs = append(resolvedResultRefs, resolved)
	}
	return resolvedResultRefs, "", nil
}

func resolveResultRef(pipelineState PipelineRunState, resultRef *v1beta1.ResultRef) (*ResolvedResultRef, string, error) {
	referencedPipelineTask := pipelineState.ToMap()[resultRef.PipelineTask]
	if referencedPipelineTask == nil {
		return nil, resultRef.PipelineTask, fmt.Errorf("could not find task %q referenced by result", resultRef.PipelineTask)
	}
	if !referencedPipelineTask.isSuccessful() && !referencedPipelineTask.isFailure() {
		return nil, resultRef.PipelineTask, fmt.Errorf("task %q referenced by result was not finished", referencedPipelineTask.PipelineTask.Name)
	}

	var runName, runValue, taskRunName string
	var resultValue v1beta1.ResultValue
	var err error
	if referencedPipelineTask.IsCustomTask() {
		if len(referencedPipelineTask.RunObjects) != 1 {
			return nil, resultRef.PipelineTask, fmt.Errorf("referenced tasks can only have length of 1 since a matrixed task does not support producing results, but was length %d", len(referencedPipelineTask.TaskRuns))
		}
		runObject := referencedPipelineTask.RunObjects[0]
		runName = runObject.GetObjectMeta().GetName()
		runValue, err = findRunResultForParam(runObject, resultRef)
		resultValue = *v1beta1.NewStructuredValues(runValue)
		if err != nil {
			return nil, resultRef.PipelineTask, err
		}
	} else {
		// Check to make sure the referenced task is not a matrix since a matrix does not support producing results
		if len(referencedPipelineTask.TaskRuns) != 1 {
			return nil, resultRef.PipelineTask, fmt.Errorf("referenced tasks can only have length of 1 since a matrixed task does not support producing results, but was length %d", len(referencedPipelineTask.TaskRuns))
		}
		taskRun := referencedPipelineTask.TaskRuns[0]
		taskRunName = taskRun.Name
		resultValue, err = findTaskResultForParam(taskRun, resultRef)
		if err != nil {
			return nil, resultRef.PipelineTask, err
		}
	}

	return &ResolvedResultRef{
		Value:           resultValue,
		FromTaskRun:     taskRunName,
		FromRun:         runName,
		ResultReference: *resultRef,
	}, "", nil
}

func findRunResultForParam(runObj v1beta1.RunObject, reference *v1beta1.ResultRef) (string, error) {
	run := runObj.(*v1beta1.CustomRun)
	for _, result := range run.Status.Results {
		if result.Name == reference.Result {
			return result.Value, nil
		}
	}
	return "", fmt.Errorf("Could not find result with name %s for task %s", reference.Result, reference.PipelineTask)
}

func findTaskResultForParam(taskRun *v1beta1.TaskRun, reference *v1beta1.ResultRef) (v1beta1.ResultValue, error) {
	results := taskRun.Status.TaskRunStatusFields.TaskRunResults
	for _, result := range results {
		if result.Name == reference.Result {
			return result.Value, nil
		}
	}
	return v1beta1.ResultValue{}, fmt.Errorf("Could not find result with name %s for task %s", reference.Result, reference.PipelineTask)
}

func (rs ResolvedResultRefs) getStringReplacements() map[string]string {
	replacements := map[string]string{}
	for _, r := range rs {
		switch r.Value.Type {
		case v1beta1.ParamTypeArray:
			for i := 0; i < len(r.Value.ArrayVal); i++ {
				for _, target := range r.getReplaceTargetfromArrayIndex(i) {
					replacements[target] = r.Value.ArrayVal[i]
				}
			}
		case v1beta1.ParamTypeObject:
			for key, element := range r.Value.ObjectVal {
				for _, target := range r.getReplaceTargetfromObjectKey(key) {
					replacements[target] = element
				}
			}

		case v1beta1.ParamTypeString:
			fallthrough
		default:
			for _, target := range r.getReplaceTarget() {
				replacements[target] = r.Value.StringVal
			}
		}
	}
	return replacements
}

func (rs ResolvedResultRefs) getArrayReplacements() map[string][]string {
	replacements := map[string][]string{}
	for _, r := range rs {
		if r.Value.Type == v1beta1.ParamType(v1beta1.ResultsTypeArray) {
			for _, target := range r.getReplaceTarget() {
				replacements[target] = r.Value.ArrayVal
			}
		}
	}
	return replacements
}

func (rs ResolvedResultRefs) getObjectReplacements() map[string]map[string]string {
	replacements := map[string]map[string]string{}
	for _, r := range rs {
		if r.Value.Type == v1beta1.ParamType(v1beta1.ResultsTypeObject) {
			for _, target := range r.getReplaceTarget() {
				replacements[target] = r.Value.ObjectVal
			}
		}
	}
	return replacements
}

func (r *ResolvedResultRef) getReplaceTarget() []string {
	return []string{
		fmt.Sprintf("%s.%s.%s.%s", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result),
		fmt.Sprintf("%s.%s.%s[%q]", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result),
		fmt.Sprintf("%s.%s.%s['%s']", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result),
	}
}

func (r *ResolvedResultRef) getReplaceTargetfromArrayIndex(idx int) []string {
	return []string{
		fmt.Sprintf("%s.%s.%s.%s[%d]", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result, idx),
		fmt.Sprintf("%s.%s.%s[%q][%d]", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result, idx),
		fmt.Sprintf("%s.%s.%s['%s'][%d]", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result, idx),
	}
}

func (r *ResolvedResultRef) getReplaceTargetfromObjectKey(key string) []string {
	return []string{
		fmt.Sprintf("%s.%s.%s.%s.%s", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result, key),
		fmt.Sprintf("%s.%s.%s[%q][%s]", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result, key),
		fmt.Sprintf("%s.%s.%s['%s'][%s]", v1beta1.ResultTaskPart, r.ResultReference.PipelineTask, v1beta1.ResultResultPart, r.ResultReference.Result, key),
	}
}
