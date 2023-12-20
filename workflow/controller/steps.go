package controller

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kubeTasker/kubeTasker/errors"
	wfv1 "github.com/kubeTasker/kubeTasker/pkg/apis/workflow/v1alpha1"
	"github.com/kubeTasker/kubeTasker/workflow/common"
	log "github.com/sirupsen/logrus"
	"github.com/valyala/fasttemplate"
)

func (woc *wfOperationCtx) executeSteps(nodeName string, tmpl *wfv1.Template) error {
	nodeID := woc.wf.NodeID(nodeName)
	defer func() {
		if woc.wf.Status.Nodes[nodeID].Completed() {
			_ = woc.killDeamonedChildren(nodeID)
		}
	}()
	scope := wfScope{
		tmpl:  tmpl,
		scope: make(map[string]interface{}),
	}
	for i, stepGroup := range tmpl.Steps {
		sgNodeName := fmt.Sprintf("%s[%d]", nodeName, i)
		woc.addChildNode(nodeName, sgNodeName)
		err := woc.executeStepGroup(stepGroup, sgNodeName, &scope)
		if err != nil {
			if errors.IsCode(errors.CodeTimeout, err) {
				return err
			}
			woc.markNodeError(nodeName, err)
			return err
		}
		sgNodeID := woc.wf.NodeID(sgNodeName)
		if !woc.wf.Status.Nodes[sgNodeID].Completed() {
			woc.log.Infof("Workflow step group node %v not yet completed", woc.wf.Status.Nodes[sgNodeID])
			return nil
		}

		if !woc.wf.Status.Nodes[sgNodeID].Successful() {
			failMessage := fmt.Sprintf("step group %s was unsuccessful", sgNodeName)
			woc.log.Info(failMessage)
			woc.markNodePhase(nodeName, wfv1.NodeFailed, failMessage)
			return nil
		}

		for _, step := range stepGroup {
			childNodeName := fmt.Sprintf("%s.%s", sgNodeName, step.Name)
			childNodeID := woc.wf.NodeID(childNodeName)
			childNode, ok := woc.wf.Status.Nodes[childNodeID]
			if !ok {
				continue
			}
			if childNode.PodIP != "" {
				key := fmt.Sprintf("steps.%s.ip", step.Name)
				scope.addParamToScope(key, childNode.PodIP)
			}
			if childNode.Outputs != nil {
				if childNode.Outputs.Result != nil {
					key := fmt.Sprintf("steps.%s.outputs.result", step.Name)
					scope.addParamToScope(key, *childNode.Outputs.Result)
				}
				for _, outParam := range childNode.Outputs.Parameters {
					key := fmt.Sprintf("steps.%s.outputs.parameters.%s", step.Name, outParam.Name)
					scope.addParamToScope(key, *outParam.Value)
				}
				for _, outArt := range childNode.Outputs.Artifacts {
					key := fmt.Sprintf("steps.%s.outputs.artifacts.%s", step.Name, outArt.Name)
					scope.addArtifactToScope(key, outArt)
				}
			}
		}
	}
	outputs, err := getTemplateOutputsFromScope(tmpl, &scope)
	if err != nil {
		woc.markNodeError(nodeName, err)
		return err
	}
	if outputs != nil {
		node := woc.wf.Status.Nodes[nodeID]
		node.Outputs = outputs
		woc.wf.Status.Nodes[nodeID] = node
	}
	woc.markNodePhase(nodeName, wfv1.NodeSucceeded)
	return nil
}

// executeStepGroup examines a map of parallel steps and executes them in parallel.
func (woc *wfOperationCtx) executeStepGroup(stepGroup []wfv1.WorkflowStep, sgNodeName string, scope *wfScope) error {
	nodeID := woc.wf.NodeID(sgNodeName)
	node, ok := woc.wf.Status.Nodes[nodeID]
	if ok && node.Completed() {
		woc.log.Debugf("Step group node %v already marked completed", node)
		return nil
	}
	if !ok {
		node = *woc.markNodePhase(sgNodeName, wfv1.NodeRunning)
		woc.log.Infof("Initializing step group node %v", node)
	}

	stepGroup, err := woc.resolveReferences(stepGroup, scope)
	if err != nil {
		woc.markNodeError(sgNodeName, err)
		return err
	}

	stepGroup, err = woc.expandStepGroup(stepGroup)
	if err != nil {
		woc.markNodeError(sgNodeName, err)
		return err
	}

	for _, step := range stepGroup {
		childNodeName := fmt.Sprintf("%s.%s", sgNodeName, step.Name)
		woc.addChildNode(sgNodeName, childNodeName)

		proceed, err := shouldExecute(step.When)
		if err != nil {
			woc.markNodeError(childNodeName, err)
			woc.markNodeError(sgNodeName, err)
			return err
		}
		if !proceed {
			skipReason := fmt.Sprintf("when '%s' evaluated false", step.When)
			woc.log.Infof("Skipping %s: %s", childNodeName, skipReason)
			woc.markNodePhase(childNodeName, wfv1.NodeSkipped, skipReason)
			continue
		}
		err = woc.executeTemplate(step.Template, step.Arguments, childNodeName)
		if err != nil {
			if !errors.IsCode(errors.CodeTimeout, err) {
				woc.markNodeError(childNodeName, err)
				woc.markNodeError(sgNodeName, err)
			}
			return err
		}
	}

	node = woc.wf.Status.Nodes[nodeID]
	for _, childNodeID := range node.Children {
		if !woc.wf.Status.Nodes[childNodeID].Completed() {
			return nil
		}
	}
	for _, childNodeID := range node.Children {
		childNode := woc.wf.Status.Nodes[childNodeID]
		if !childNode.Successful() {
			failMessage := fmt.Sprintf("child '%s' failed", childNodeID)
			woc.markNodePhase(sgNodeName, wfv1.NodeFailed, failMessage)
			woc.log.Infof("Step group node %s deemed failed: %s", childNode, failMessage)
			return nil
		}
	}
	woc.markNodePhase(node.Name, wfv1.NodeSucceeded)
	woc.log.Infof("Step group node %v successful", woc.wf.Status.Nodes[nodeID])
	return nil
}

var whenExpression = regexp.MustCompile("^(.*)(==|!=)(.*)$")

// shouldExecute evaluates a already substituted when expression to decide whether or not a step should execute
func shouldExecute(when string) (bool, error) {
	if when == "" {
		return true, nil
	}
	parts := whenExpression.FindStringSubmatch(when)
	if len(parts) == 0 {
		return false, errors.Errorf(errors.CodeBadRequest, "Invalid 'when' expression: %s", when)
	}
	var1 := strings.TrimSpace(parts[1])
	operator := parts[2]
	var2 := strings.TrimSpace(parts[3])
	switch operator {
	case "==":
		return var1 == var2, nil
	case "!=":
		return var1 != var2, nil
	default:
		return false, errors.Errorf(errors.CodeBadRequest, "Unknown operator: %s", operator)
	}
}

// resolveReferences replaces any references to outputs of previous steps, or artifacts in the inputs
func (woc *wfOperationCtx) resolveReferences(stepGroup []wfv1.WorkflowStep, scope *wfScope) ([]wfv1.WorkflowStep, error) {
	newStepGroup := make([]wfv1.WorkflowStep, len(stepGroup))

	for i, step := range stepGroup {
		stepBytes, err := json.Marshal(step)
		if err != nil {
			return nil, errors.InternalWrapError(err)
		}
		replaceMap := make(map[string]string)
		for key, val := range scope.scope {
			valStr, ok := val.(string)
			if ok {
				replaceMap[key] = valStr
			}
		}
		fstTmpl := fasttemplate.New(string(stepBytes), "{{", "}}")
		newStepStr, err := common.Replace(fstTmpl, replaceMap, true, "")
		if err != nil {
			return nil, err
		}
		var newStep wfv1.WorkflowStep
		err = json.Unmarshal([]byte(newStepStr), &newStep)
		if err != nil {
			return nil, errors.InternalWrapError(err)
		}

		for j, art := range newStep.Arguments.Artifacts {
			if art.From == "" {
				continue
			}
			resolvedArt, err := scope.resolveArtifact(art.From)
			if err != nil {
				return nil, err
			}
			resolvedArt.Name = art.Name
			newStep.Arguments.Artifacts[j] = *resolvedArt
		}

		newStepGroup[i] = newStep
	}
	return newStepGroup, nil
}

// expandStepGroup looks at each step in a collection of parallel steps, and expands all steps using withItems/withParam
func (woc *wfOperationCtx) expandStepGroup(stepGroup []wfv1.WorkflowStep) ([]wfv1.WorkflowStep, error) {
	newStepGroup := make([]wfv1.WorkflowStep, 0)
	for _, step := range stepGroup {
		if len(step.WithItems) == 0 && step.WithParam == "" {
			newStepGroup = append(newStepGroup, step)
			continue
		}
		expandedStep, err := woc.expandStep(step)
		if err != nil {
			return nil, err
		}

		newStepGroup = append(newStepGroup, expandedStep...)
	}
	return newStepGroup, nil
}

// expandStep expands a step containing withItems or withParams into multiple parallel steps
func (woc *wfOperationCtx) expandStep(step wfv1.WorkflowStep) ([]wfv1.WorkflowStep, error) {
	stepBytes, err := json.Marshal(step)
	if err != nil {
		return nil, errors.InternalWrapError(err)
	}
	fstTmpl := fasttemplate.New(string(stepBytes), "{{", "}}")
	expandedStep := make([]wfv1.WorkflowStep, 0)
	var items []wfv1.Item
	if len(step.WithItems) > 0 {
		items = step.WithItems
	} else if step.WithParam != "" {
		err = json.Unmarshal([]byte(step.WithParam), &items)
		if err != nil {
			return nil, errors.Errorf(errors.CodeBadRequest, "withParam value not be parsed as a JSON list: %s", step.WithParam)
		}
	} else {
		return nil, errors.InternalError("expandStep() was called with withItems and withParam empty")
	}

	for i, item := range items {
		var newStep wfv1.WorkflowStep
		newStepName, err := processItem(fstTmpl, step.Name, i, item, &newStep)
		if err != nil {
			return nil, err
		}
		newStep.Name = newStepName
		newStep.Template = step.Template
		expandedStep = append(expandedStep, newStep)
	}
	return expandedStep, nil
}

// killDeamonedChildren kill any granchildren of a step template node, which have been daemoned.
func (woc *wfOperationCtx) killDeamonedChildren(nodeID string) error {
	woc.log.Infof("Checking deamon children of %s", nodeID)
	var firstErr error
	execCtl := common.ExecutionControl{
		Deadline: &time.Time{},
	}
	for _, childNodeID := range woc.wf.Status.Nodes[nodeID].Children {
		for _, grandChildID := range woc.wf.Status.Nodes[childNodeID].Children {
			gcNode := woc.wf.Status.Nodes[grandChildID]
			if gcNode.Daemoned == nil || !*gcNode.Daemoned {
				continue
			}
			err := woc.updateExecutionControl(gcNode.ID, execCtl)
			if err != nil {
				woc.log.Errorf("Failed to update execution control of %s: %+v", gcNode, err)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// updateExecutionControl updates the execution control parameters
func (woc *wfOperationCtx) updateExecutionControl(podName string, execCtl common.ExecutionControl) error {
	execCtlBytes, err := json.Marshal(execCtl)
	if err != nil {
		return errors.InternalWrapError(err)
	}

	woc.log.Infof("Updating execution control of %s: %s", podName, execCtlBytes)
	err = common.AddPodAnnotation(
		woc.controller.kubeclientset,
		podName,
		woc.wf.ObjectMeta.Namespace,
		common.AnnotationKeyExecutionControl,
		string(execCtlBytes),
	)
	if err != nil {
		return err
	}

	woc.log.Infof("Signalling %s of updates", podName)
	exec, err := common.ExecPodContainer(
		woc.controller.restConfig, woc.wf.ObjectMeta.Namespace, podName,
		common.WaitContainerName, true, true, "sh", "-c", "kill -s USR2 1",
	)
	if err != nil {
		return err
	}
	go func() {
		_, _, err = common.GetExecutorOutput(exec)
		if err != nil {
			log.Warnf("Signal command failed: %v", err)
			return
		}
		log.Infof("Signal of %s (%s) successfully issued", podName, common.WaitContainerName)
	}()

	return nil
}
