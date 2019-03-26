/*
Copyright 2018 BlackRock, Inc.

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

package sensors

import (
	"encoding/json"
	"fmt"

	"github.com/Knetic/govaluate"

	"github.com/argoproj/argo-events/common"
	sn "github.com/argoproj/argo-events/controllers/sensor"
	apicommon "github.com/argoproj/argo-events/pkg/apis/common"
	"github.com/argoproj/argo-events/pkg/apis/sensor"
	"github.com/argoproj/argo-events/pkg/apis/sensor/v1alpha1"
	"github.com/argoproj/argo-events/store"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// canProcessTriggers evaluates whether triggers can be processed and executed
func (sec *sensorExecutionCtx) canProcessTriggers() (bool, error) {
	if sec.sensor.Spec.DependencyGroups != nil {
		groups := make(map[string]interface{}, len(sec.sensor.Spec.DependencyGroups))
	group:
		for _, group := range sec.sensor.Spec.DependencyGroups {
			for _, dependency := range group.Dependencies {
				if nodeStatus := sn.GetNodeByName(sec.sensor, dependency); nodeStatus.Phase != v1alpha1.NodePhaseComplete {
					groups[group.Name] = false
					continue group
				}
			}
			sn.MarkNodePhase(sec.sensor, group.Name, v1alpha1.NodeTypeDependencyGroup, v1alpha1.NodePhaseComplete, nil, sec.log, "dependency group is complete")
			groups[group.Name] = true
		}

		expression, err := govaluate.NewEvaluableExpression(sec.sensor.Spec.Circuit)
		if err != nil {
			return false, fmt.Errorf("failed to create circuit expression. err: %+v", err)
		}

		result, err := expression.Evaluate(groups)
		if err != nil {
			return false, fmt.Errorf("failed to evaluate circuit expression. err: %+v", err)
		}

		return result == true, err
	}

	if sec.sensor.AreAllNodesSuccess(v1alpha1.NodeTypeEventDependency) {
		return true, nil
	}

	return false, nil
}

// canExecuteTrigger determines whether a trigger is executable based on condition set on trigger
func (sec *sensorExecutionCtx) canExecuteTrigger(trigger v1alpha1.Trigger) bool {
	if trigger.Template.When == nil {
		return true
	}
	if trigger.Template.When.Any != nil {
		for _, group := range trigger.Template.When.Any {
			if status := sn.GetNodeByName(sec.sensor, group); status.Type == v1alpha1.NodeTypeDependencyGroup && status.Phase == v1alpha1.NodePhaseComplete {
				return true
			}
			return false
		}
	}
	if trigger.Template.When.All != nil {
		for _, group := range trigger.Template.When.All {
			if status := sn.GetNodeByName(sec.sensor, group); status.Type == v1alpha1.NodeTypeDependencyGroup && status.Phase != v1alpha1.NodePhaseComplete {
				return false
			}
			return true
		}
	}
	return true
}

// processTriggers checks if all event dependencies are complete and then starts executing triggers
func (sec *sensorExecutionCtx) processTriggers() {
	// to trigger the sensor action/s we need to check if all event dependencies are completed and sensor is active
	sec.log.Info("all event dependencies are marked completed, processing triggers")

	// labels for K8s event
	labels := map[string]string{
		common.LabelSensorName: sec.sensor.Name,
		common.LabelOperation:  "process_triggers",
	}

	for _, trigger := range sec.sensor.Spec.Triggers {
		log := sec.log.WithField("trigger", trigger.Template.Name)
		// check if a trigger condition is set
		if canExecute := sec.canExecuteTrigger(trigger); !canExecute {
			log.Info("trigger can't be executed because trigger condition failed")
			continue
		}

		if err := sec.executeTrigger(trigger); err != nil {
			log.WithError(err).Error("trigger failed to execute")

			sn.MarkNodePhase(sec.sensor, trigger.Template.Name, v1alpha1.NodeTypeTrigger, v1alpha1.NodePhaseError, nil, sec.log, fmt.Sprintf("failed to execute trigger.Template. err: %+v", err))

			// escalate using K8s event
			labels[common.LabelEventType] = string(common.EscalationEventType)
			if err := common.GenerateK8sEvent(sec.kubeClient, fmt.Sprintf("failed to execute trigger %s", trigger.Template.Name), common.EscalationEventType,
				"trigger failure", sec.sensor.Name, sec.sensor.Namespace, sec.controllerInstanceID, sensor.Kind, labels); err != nil {
				log.WithError(err).Error("failed to create K8s event to escalate trigger failure")
			}
			continue
		}

		// mark trigger as complete.
		sn.MarkNodePhase(sec.sensor, trigger.Template.Name, v1alpha1.NodeTypeTrigger, v1alpha1.NodePhaseComplete, nil, sec.log, "successfully executed trigger")

		labels[common.LabelEventType] = string(common.OperationSuccessEventType)
		if err := common.GenerateK8sEvent(sec.kubeClient, fmt.Sprintf("trigger %s executed successfully", trigger.Template.Name), common.OperationSuccessEventType,
			"trigger executed", sec.sensor.Name, sec.sensor.Namespace, sec.controllerInstanceID, sensor.Kind, labels); err != nil {
			sec.log.WithError(err).Error("failed to create K8s event to log trigger execution")
		}
	}

	// increment completion counter
	sec.sensor.Status.CompletionCount = sec.sensor.Status.CompletionCount + 1

	// create K8s event to mark the trigger round completion
	labels[common.LabelEventType] = string(common.OperationSuccessEventType)
	if err := common.GenerateK8sEvent(sec.kubeClient, fmt.Sprintf("completion count:%d", sec.sensor.Status.CompletionCount), common.OperationSuccessEventType,
		"triggers execution round completion", sec.sensor.Name, sec.sensor.Namespace, sec.controllerInstanceID, sensor.Kind, labels); err != nil {
		sec.log.WithError(err).Error("failed to create K8s event to log trigger execution round completion")
	}

	// Mark all dependency nodes as active
	for _, dependency := range sec.sensor.Spec.Dependencies {
		sn.MarkNodePhase(sec.sensor, dependency.Name, v1alpha1.NodeTypeEventDependency, v1alpha1.NodePhaseActive, nil, sec.log, "dependency is re-activated")
	}
	// Mark all dependency groups as active
	for _, group := range sec.sensor.Spec.DependencyGroups {
		sn.MarkNodePhase(sec.sensor, group.Name, v1alpha1.NodeTypeDependencyGroup, v1alpha1.NodePhaseActive, nil, sec.log, "dependency group is re-activated")
	}
}

// apply parameters for trigger
func (sec *sensorExecutionCtx) applyParamsTrigger(trigger *v1alpha1.Trigger) error {
	if trigger.TemplateParameters != nil && len(trigger.TemplateParameters) > 0 {
		templateBytes, err := json.Marshal(trigger.Template)
		if err != nil {
			return err
		}
		tObj, err := applyParams(templateBytes, trigger.TemplateParameters, sec.extractEvents(trigger.TemplateParameters))
		if err != nil {
			return err
		}
		template := &v1alpha1.TriggerTemplate{}
		if err = json.Unmarshal(tObj, template); err != nil {
			return err
		}
		trigger.Template = template
	}
	return nil
}

func (sec *sensorExecutionCtx) applyParamsResource(parameters []v1alpha1.TriggerParameter, obj *unstructured.Unstructured) error {
	if parameters != nil && len(parameters) > 0 {
		jObj, err := obj.MarshalJSON()
		if err != nil {
			return fmt.Errorf("failed to marshal json. err: %+v", err)
		}
		events := sec.extractEvents(parameters)
		jUpdatedObj, err := applyParams(jObj, parameters, events)
		if err != nil {
			return fmt.Errorf("failed to apply params. err: %+v", err)
		}
		err = obj.UnmarshalJSON(jUpdatedObj)
		if err != nil {
			return fmt.Errorf("failed to un-marshal json. err: %+v", err)
		}
	}
	return nil
}

// execute the trigger
func (sec *sensorExecutionCtx) executeTrigger(trigger v1alpha1.Trigger) error {
	if trigger.Template != nil {
		if err := sec.applyParamsTrigger(&trigger); err != nil {
			return err
		}
		creds, err := store.GetCredentials(sec.kubeClient, sec.sensor.Namespace, trigger.Template.Source)
		if err != nil {
			return err
		}
		reader, err := store.GetArtifactReader(trigger.Template.Source, creds, sec.kubeClient)
		if err != nil {
			return err
		}
		uObj, err := store.FetchArtifact(reader, trigger.Template.GroupVersionKind)
		if err != nil {
			return err
		}
		if err = sec.createResourceObject(trigger.Template, trigger.ResourceParameters, uObj); err != nil {
			return err
		}
	}
	return nil
}

// createResourceObject creates K8s object for trigger
func (sec *sensorExecutionCtx) createResourceObject(resource *v1alpha1.TriggerTemplate, parameters []v1alpha1.TriggerParameter, obj *unstructured.Unstructured) error {
	namespace := obj.GetNamespace()
	// Defaults to sensor's namespace
	if namespace == "" {
		namespace = sec.sensor.Namespace
	}
	obj.SetNamespace(namespace)

	// passing parameters to the resource object requires 4 steps
	// 1. marshaling the obj to JSON
	// 2. extract the appropriate eventDependency events based on the resource params
	// 3. apply the params to the JSON object
	// 4. unmarshal the obj from the updated JSON
	if err := sec.applyParamsResource(parameters, obj); err != nil {
		return err
	}

	gvk := obj.GroupVersionKind()
	client, err := sec.clientPool.ClientForGroupVersionKind(gvk)
	if err != nil {
		return fmt.Errorf("failed to get client for given group version and kind. err: %+v", err)
	}

	apiResource, err := common.ServerResourceForGroupVersionKind(sec.discoveryClient, gvk)
	if err != nil {
		return fmt.Errorf("failed to get server resource for given group version and kind. err: %+v", err)
	}

	reIf := client.Resource(apiResource, namespace)
	nObj, err := reIf.Create(obj)
	if err != nil {
		return fmt.Errorf("failed to create resource object. err: %+v", err)
	}

	log := sec.log.WithFields(
		map[string]interface{}{
			"name":               nObj.GetName(),
			"group-version-kind": gvk.String(),
		},
	)

	log.Info("resource created")
	if err == nil || !errors.IsAlreadyExists(err) {
		return err
	}
	_, err = reIf.Get(obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return err
	}
	log.Warn("object already exist")
	return nil
}

// helper method to extract the events from the event dependencies nodes associated with the resource params
// returns a map of the events keyed by the event dependency name
func (sec *sensorExecutionCtx) extractEvents(params []v1alpha1.TriggerParameter) map[string]apicommon.Event {
	events := make(map[string]apicommon.Event)
	for _, param := range params {
		log := sec.log.WithFields(
			map[string]interface{}{
				"event": param.Src.Event,
				"dest":  param.Dest,
			},
		)
		if param.Src != nil {
			node := sn.GetNodeByName(sec.sensor, param.Src.Event)
			if node == nil {
				log.Warn("event dependency node does not exist, cannot apply parameter")
				continue
			}
			if node.Event == nil {
				log.Warn("event in event dependency does not exist, cannot apply parameter")
				continue
			}
			events[param.Src.Event] = *node.Event
		}
	}
	return events
}
