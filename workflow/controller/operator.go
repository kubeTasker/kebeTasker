package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/kubeTasker/kubeTasker/errors"
	workflow "github.com/kubeTasker/kubeTasker/pkg/apis/workflow"
	wfv1 "github.com/kubeTasker/kubeTasker/pkg/apis/workflow/v1alpha1"
	"github.com/kubeTasker/kubeTasker/workflow/common"
	log "github.com/sirupsen/logrus"
	"github.com/valyala/fasttemplate"
	apiv1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
)

// wfOperationCtx is the context for evaluation and operation of a single workflow
type wfOperationCtx struct {
	wf            *wfv1.Workflow
	orig          *wfv1.Workflow
	updated       bool
	log           *log.Entry
	controller    *WorkflowController
	globalParams  map[string]string
	completedPods map[string]bool
	deadline      time.Time
}

const maxOperationTime time.Duration = 10 * time.Second

type wfScope struct {
	tmpl  *wfv1.Template
	scope map[string]interface{}
}

// newWorkflowOperationCtx creates and initializes a new wfOperationCtx object.
func newWorkflowOperationCtx(wf *wfv1.Workflow, wfc *WorkflowController) *wfOperationCtx {
	woc := wfOperationCtx{
		wf:      wf.DeepCopyObject().(*wfv1.Workflow),
		orig:    wf,
		updated: false,
		log: log.WithFields(log.Fields{
			"workflow":  wf.ObjectMeta.Name,
			"namespace": wf.ObjectMeta.Namespace,
		}),
		controller:    wfc,
		globalParams:  make(map[string]string),
		completedPods: make(map[string]bool),
		deadline:      time.Now().UTC().Add(maxOperationTime),
	}

	if woc.wf.Status.Nodes == nil {
		woc.wf.Status.Nodes = make(map[string]wfv1.NodeStatus)
	}

	return &woc
}

// operate is the main operator logic of a workflow.
func (woc *wfOperationCtx) operate() {
	defer woc.persistUpdates()
	defer func() {
		if r := recover(); r != nil {
			if rerr, ok := r.(error); ok {
				woc.markWorkflowError(rerr, true)
			} else {
				woc.markWorkflowPhase(wfv1.NodeError, true, fmt.Sprintf("%v", r))
			}
			woc.log.Errorf("Recovered from panic: %+v\n%s", r, debug.Stack())
		}
	}()
	woc.log.Infof("Processing workflow")
	if woc.wf.Status.Phase == "" {
		woc.markWorkflowRunning()
		err := common.ValidateWorkflow(woc.wf)
		if err != nil {
			woc.markWorkflowFailed(fmt.Sprintf("invalid spec: %s", err.Error()))
			return
		}
	} else {
		err := woc.podReconciliation()
		if err != nil {
			woc.log.Errorf("%s error: %+v", woc.wf.ObjectMeta.Name, err)
			return
		}
	}
	woc.globalParams[common.GlobalVarWorkflowName] = woc.wf.ObjectMeta.Name
	woc.globalParams[common.GlobalVarWorkflowUID] = string(woc.wf.ObjectMeta.UID)
	for _, param := range woc.wf.Spec.Arguments.Parameters {
		woc.globalParams["workflow.parameters."+param.Name] = *param.Value
	}

	err := woc.createPVCs()
	if err != nil {
		woc.log.Errorf("%s error: %+v", woc.wf.ObjectMeta.Name, err)
		woc.markWorkflowError(err, true)
		return
	}
	err = woc.executeTemplate(woc.wf.Spec.Entrypoint, woc.wf.Spec.Arguments, woc.wf.ObjectMeta.Name)
	if err != nil {
		if errors.IsCode(errors.CodeTimeout, err) {
			woc.requeue()
			return
		}
		woc.log.Errorf("%s error: %+v", woc.wf.ObjectMeta.Name, err)
	}
	node := woc.wf.Status.Nodes[woc.wf.NodeID(woc.wf.ObjectMeta.Name)]
	if !node.Completed() {
		return
	}

	var onExitNode *wfv1.NodeStatus
	if woc.wf.Spec.OnExit != "" {
		if node.Phase == wfv1.NodeSkipped {
			woc.globalParams[common.GlobalVarWorkflowStatus] = string(wfv1.NodeSucceeded)
		} else {
			woc.globalParams[common.GlobalVarWorkflowStatus] = string(node.Phase)
		}
		woc.log.Infof("Running OnExit handler: %s", woc.wf.Spec.OnExit)
		onExitNodeName := woc.wf.ObjectMeta.Name + ".onExit"
		err = woc.executeTemplate(woc.wf.Spec.OnExit, woc.wf.Spec.Arguments, onExitNodeName)
		if err != nil {
			if errors.IsCode(errors.CodeTimeout, err) {
				woc.requeue()
				return
			}
			woc.log.Errorf("%s error: %+v", onExitNodeName, err)
		}
		xitNode := woc.wf.Status.Nodes[woc.wf.NodeID(onExitNodeName)]
		onExitNode = &xitNode
		if !onExitNode.Completed() {
			return
		}
	}

	err = woc.deletePVCs()
	if err != nil {
		woc.log.Errorf("%s error: %+v", woc.wf.ObjectMeta.Name, err)
		woc.markWorkflowError(err, false)
		return
	}

	switch node.Phase {
	case wfv1.NodeSucceeded, wfv1.NodeSkipped:
		if onExitNode != nil && !onExitNode.Successful() {
			woc.markWorkflowPhase(onExitNode.Phase, true, onExitNode.Message)
		} else {
			woc.markWorkflowSuccess()
		}
	case wfv1.NodeFailed:
		woc.markWorkflowFailed(node.Message)
	case wfv1.NodeError:
		woc.markWorkflowPhase(wfv1.NodeError, true, node.Message)
	default:
		err = errors.InternalErrorf("Unexpected node phase %s: %+v", woc.wf.ObjectMeta.Name, err)
		woc.markWorkflowError(err, true)
	}
}

// persistUpdates will update a workflow with any updates made during workflow operation.
func (woc *wfOperationCtx) persistUpdates() {
	if !woc.updated {
		return
	}
	oldData, err := json.Marshal(woc.orig)
	if err != nil {
		woc.log.Errorf("Error marshalling orig wf for patch: %+v", err)
		return
	}
	newData, err := json.Marshal(woc.wf)
	if err != nil {
		woc.log.Errorf("Error marshalling wf for patch: %+v", err)
		return
	}
	patchBytes, err := jsonpatch.CreateMergePatch(oldData, newData)
	if err != nil {
		woc.log.Errorf("Error creating patch: %+v", err)
		return
	}
	if string(patchBytes) != "{}" {
		woc.log.Debugf("Applying patch: %s", patchBytes)
		wfClient := woc.controller.wfclientset.KubetaskerV1alpha1().Workflows(woc.wf.ObjectMeta.Namespace)
		_, err = wfClient.Patch(context.TODO(), woc.wf.ObjectMeta.Name, types.MergePatchType, patchBytes, metav1.PatchOptions{})
		if err != nil {
			woc.log.Errorf("Error applying patch %s: %v", patchBytes, err)
			return
		}
		woc.log.Info("Patch successful")
		time.Sleep(1 * time.Second)
	}

	for podName := range woc.completedPods {
		woc.controller.completedPods <- fmt.Sprintf("%s/%s", woc.wf.ObjectMeta.Namespace, podName)
	}
}

// requeue this workflow onto the workqueue for later processing
func (woc *wfOperationCtx) requeue() {
	key, err := cache.MetaNamespaceKeyFunc(woc.wf)
	if err != nil {
		woc.log.Errorf("Failed to requeue workflow %s: %v", woc.wf.ObjectMeta.Name, err)
		return
	}
	woc.controller.wfQueue.Add(key)
}

func (woc *wfOperationCtx) processNodeRetries(node *wfv1.NodeStatus) error {
	if node.Completed() {
		return nil
	}
	lastChildNode, err := woc.getLastChildNode(node)
	if err != nil {
		return fmt.Errorf("Failed to find last child of node " + node.Name)
	}

	if lastChildNode == nil {
		return nil
	}

	if !lastChildNode.Completed() {
		return nil
	}

	if lastChildNode.Successful() {
		node.Outputs = lastChildNode.Outputs.DeepCopy()
		woc.wf.Status.Nodes[node.ID] = *node
		woc.markNodePhase(node.Name, wfv1.NodeSucceeded)
		return nil
	}

	if !lastChildNode.CanRetry() {
		woc.log.Infof("Node cannot be retried. Marking it failed")
		woc.markNodePhase(node.Name, wfv1.NodeFailed, lastChildNode.Message)
		return nil
	}

	if node.RetryStrategy.Limit != nil && int32(len(node.Children)) > *node.RetryStrategy.Limit {
		woc.log.Infoln("No more retries left. Failing...")
		woc.markNodePhase(node.Name, wfv1.NodeFailed, "No more retries left")
		return nil
	}

	woc.log.Infof("%d child nodes of %s failed. Trying again...", len(node.Children), node.Name)
	return nil
}

// podReconciliation is the process by which a workflow will examine all its related
func (woc *wfOperationCtx) podReconciliation() error {
	podList, err := woc.getAllWorkflowPods()
	if err != nil {
		return err
	}
	seenPods := make(map[string]bool)
	for _, pod := range podList.Items {
		nodeNameForPod := pod.Annotations[common.AnnotationKeyNodeName]
		nodeID := woc.wf.NodeID(nodeNameForPod)
		seenPods[nodeID] = true
		if node, ok := woc.wf.Status.Nodes[nodeID]; ok {
			if newState := assessNodeStatus(&pod, &node); newState != nil {
				woc.wf.Status.Nodes[nodeID] = *newState
				woc.updated = true
			}
			if woc.wf.Status.Nodes[pod.ObjectMeta.Name].Completed() {
				woc.completedPods[pod.ObjectMeta.Name] = true
			}
		}
	}

	if len(podList.Items) > 0 {
		return nil
	}
	woc.log.Info("Checking for deleted pods")
	podList, err = woc.getAllWorkflowPods()
	if err != nil {
		return err
	}
	for _, pod := range podList.Items {
		nodeNameForPod := pod.Annotations[common.AnnotationKeyNodeName]
		nodeID := woc.wf.NodeID(nodeNameForPod)
		seenPods[nodeID] = true
		if node, ok := woc.wf.Status.Nodes[nodeID]; ok {
			if newState := assessNodeStatus(&pod, &node); newState != nil {
				woc.wf.Status.Nodes[nodeID] = *newState
				woc.updated = true
			}
			if woc.wf.Status.Nodes[pod.ObjectMeta.Name].Completed() {
				woc.completedPods[pod.ObjectMeta.Name] = true
			}
		}
	}

	for nodeID, node := range woc.wf.Status.Nodes {
		if len(node.Children) > 0 || node.Completed() {
			continue
		}

		if _, ok := seenPods[nodeID]; !ok {
			node.Message = "pod deleted"
			node.Phase = wfv1.NodeError
			woc.wf.Status.Nodes[nodeID] = node
			woc.log.Warnf("pod %s deleted", nodeID)
			woc.updated = true
		}
	}
	return nil
}

// getAllWorkflowPods returns all pods related to the current workflow
func (woc *wfOperationCtx) getAllWorkflowPods() (*apiv1.PodList, error) {
	options := metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s",
			common.LabelKeyWorkflow,
			woc.wf.ObjectMeta.Name),
	}
	podList, err := woc.controller.kubeclientset.CoreV1().Pods(woc.wf.Namespace).List(context.TODO(), options)
	if err != nil {
		return nil, errors.InternalWrapError(err)
	}
	return podList, nil
}

// assessNodeStatus compares the current state of a pod with its corresponding node
// and returns the new node status if something changed
func assessNodeStatus(pod *apiv1.Pod, node *wfv1.NodeStatus) *wfv1.NodeStatus {
	var newPhase wfv1.NodePhase
	var newDaemonStatus *bool
	var message string
	updated := false
	f := false
	switch pod.Status.Phase {
	case apiv1.PodPending:
		return nil
	case apiv1.PodSucceeded:
		newPhase = wfv1.NodeSucceeded
		newDaemonStatus = &f
	case apiv1.PodFailed:
		newPhase, message = inferFailedReason(pod)
		newDaemonStatus = &f
	case apiv1.PodRunning:
		tmplStr, ok := pod.Annotations[common.AnnotationKeyTemplate]
		if !ok {
			log.Warnf("%s missing template annotation", pod.ObjectMeta.Name)
			return nil
		}
		var tmpl wfv1.Template
		err := json.Unmarshal([]byte(tmplStr), &tmpl)
		if err != nil {
			log.Warnf("%s template annotation unreadable: %v", pod.ObjectMeta.Name, err)
			return nil
		}
		if tmpl.Daemon == nil || !*tmpl.Daemon {
			return nil
		}
		for _, ctrStatus := range pod.Status.ContainerStatuses {
			if !ctrStatus.Ready {
				return nil
			}
		}
		newPhase = wfv1.NodeSucceeded
		t := true
		newDaemonStatus = &t
		log.Infof("Processing ready daemon pod: %v", pod.ObjectMeta.SelfLink)
	default:
		newPhase = wfv1.NodeError
		message = fmt.Sprintf("Unexpected pod phase for %s: %s", pod.ObjectMeta.Name, pod.Status.Phase)
		log.Error(message)
	}

	if newDaemonStatus != nil {
		if *newDaemonStatus == false {
			newDaemonStatus = nil
		}
		if (newDaemonStatus != nil && node.Daemoned == nil) || (newDaemonStatus == nil && node.Daemoned != nil) {
			log.Infof("Setting node %v daemoned: %v -> %v", node, node.Daemoned, newDaemonStatus)
			node.Daemoned = newDaemonStatus
			updated = true
			if pod.Status.PodIP != "" && pod.Status.PodIP != node.PodIP {
				log.Infof("Updating daemon node %s IP %s -> %s", node, node.PodIP, pod.Status.PodIP)
				node.PodIP = pod.Status.PodIP
			}
		}
	}
	outputStr, ok := pod.Annotations[common.AnnotationKeyOutputs]
	if ok && node.Outputs == nil {
		updated = true
		log.Infof("Setting node %v outputs", node)
		var outputs wfv1.Outputs
		err := json.Unmarshal([]byte(outputStr), &outputs)
		if err != nil {
			log.Errorf("Failed to unmarshal %s outputs from pod annotation: %v", pod.Name, err)
			node.Phase = wfv1.NodeError
		} else {
			node.Outputs = &outputs
		}
	}
	if message != "" && node.Message != message {
		log.Infof("Updating node %s message: %s", node, message)
		node.Message = message
	}
	if node.Phase != newPhase {
		log.Infof("Updating node %s status %s -> %s", node, node.Phase, newPhase)
		updated = true
		node.Phase = newPhase
	}
	if node.Completed() && node.FinishedAt.IsZero() {
		updated = true
		if !node.IsDaemoned() {
			node.FinishedAt = getLatestFinishedAt(pod)
		}
		if node.FinishedAt.IsZero() {
			node.FinishedAt = metav1.Time{Time: time.Now().UTC()}
		}
	}
	if updated {
		return node
	}
	return nil
}

// getLatestFinishedAt returns the latest finishAt timestamp from all the
// containers of this pod.
func getLatestFinishedAt(pod *apiv1.Pod) metav1.Time {
	var latest metav1.Time
	for _, ctr := range pod.Status.InitContainerStatuses {
		if ctr.State.Terminated != nil && ctr.State.Terminated.FinishedAt.After(latest.Time) {
			latest = ctr.State.Terminated.FinishedAt
		}
	}
	for _, ctr := range pod.Status.ContainerStatuses {
		if ctr.State.Terminated != nil && ctr.State.Terminated.FinishedAt.After(latest.Time) {
			latest = ctr.State.Terminated.FinishedAt
		}
	}
	return latest
}

// inferFailedReason returns metadata about a Failed pod to be used in its NodeStatus
// Returns a tuple of the new phase and message
func inferFailedReason(pod *apiv1.Pod) (wfv1.NodePhase, string) {
	if pod.Status.Message != "" {
		return wfv1.NodeFailed, pod.Status.Message
	}
	annotatedMsg := pod.Annotations[common.AnnotationKeyNodeMessage]
	for _, ctr := range pod.Status.InitContainerStatuses {
		if ctr.State.Terminated == nil {
			log.Warnf("Pod %s phase was Failed but %s did not have terminated state", pod.ObjectMeta.Name, ctr.Name)
			continue
		}
		if ctr.State.Terminated.ExitCode == 0 {
			continue
		}
		errMsg := "failed to load artifacts"
		for _, msg := range []string{annotatedMsg, ctr.State.Terminated.Message} {
			if msg != "" {
				errMsg += ": " + msg
				break
			}
		}
		return wfv1.NodeError, errMsg
	}
	failMessages := make(map[string]string)
	for _, ctr := range pod.Status.ContainerStatuses {
		if ctr.State.Terminated == nil {
			log.Warnf("Pod %s phase was Failed but %s did not have terminated state", pod.ObjectMeta.Name, ctr.Name)
			continue
		}
		if ctr.State.Terminated.ExitCode == 0 {
			continue
		}
		if ctr.Name == common.WaitContainerName {
			errDetails := ""
			for _, msg := range []string{annotatedMsg, ctr.State.Terminated.Message} {
				if msg != "" {
					errDetails = msg
					break
				}
			}
			if errDetails == "" {
				errDetails = fmt.Sprintf("verify serviceaccount %s:%s has necessary privileges", pod.ObjectMeta.Namespace, pod.Spec.ServiceAccountName)
			}
			errMsg := fmt.Sprintf("failed to save outputs: %s", errDetails)
			failMessages[ctr.Name] = errMsg
		} else {
			if ctr.State.Terminated.Message != "" {
				failMessages[ctr.Name] = ctr.State.Terminated.Message
			} else {
				errMsg := fmt.Sprintf("failed with exit code %d", ctr.State.Terminated.ExitCode)
				if ctr.Name != common.MainContainerName {
					errMsg = fmt.Sprintf("sidecar '%s' %s", ctr.Name, errMsg)
				}
				failMessages[ctr.Name] = errMsg
			}
		}
	}
	if failMsg, ok := failMessages[common.MainContainerName]; ok {
		_, ok = failMessages[common.WaitContainerName]
		isResourceTemplate := !ok
		if isResourceTemplate && annotatedMsg != "" {
			return wfv1.NodeFailed, annotatedMsg
		}
		return wfv1.NodeFailed, failMsg
	}
	if failMsg, ok := failMessages[common.WaitContainerName]; ok {
		return wfv1.NodeError, failMsg
	}

	for _, failMsg := range failMessages {
		return wfv1.NodeFailed, failMsg
	}
	return wfv1.NodeFailed, "pod failed for unknown reason"
}

func (woc *wfOperationCtx) createPVCs() error {
	if woc.wf.Status.Phase != wfv1.NodeRunning {
		return nil
	}
	if len(woc.wf.Spec.VolumeClaimTemplates) == len(woc.wf.Status.PersistentVolumeClaims) {
		return nil
	}
	if len(woc.wf.Status.PersistentVolumeClaims) == 0 {
		woc.wf.Status.PersistentVolumeClaims = make([]apiv1.Volume, len(woc.wf.Spec.VolumeClaimTemplates))
	}
	pvcClient := woc.controller.kubeclientset.CoreV1().PersistentVolumeClaims(woc.wf.ObjectMeta.Namespace)
	for i, pvcTmpl := range woc.wf.Spec.VolumeClaimTemplates {
		if pvcTmpl.ObjectMeta.Name == "" {
			return errors.Errorf(errors.CodeBadRequest, "volumeClaimTemplates[%d].metadata.name is required", i)
		}
		pvcTmpl = *pvcTmpl.DeepCopy()
		refName := pvcTmpl.ObjectMeta.Name
		pvcName := fmt.Sprintf("%s-%s", woc.wf.ObjectMeta.Name, pvcTmpl.ObjectMeta.Name)
		woc.log.Infof("Creating pvc %s", pvcName)
		pvcTmpl.ObjectMeta.Name = pvcName
		pvcTmpl.OwnerReferences = []metav1.OwnerReference{
			*metav1.NewControllerRef(woc.wf, wfv1.SchemeGroupVersion.WithKind(workflow.Kind)),
		}
		pvc, err := pvcClient.Create(context.TODO(), &pvcTmpl, metav1.CreateOptions{})
		if err != nil {
			woc.markNodeError(woc.wf.ObjectMeta.Name, err)
			return err
		}
		vol := apiv1.Volume{
			Name: refName,
			VolumeSource: apiv1.VolumeSource{
				PersistentVolumeClaim: &apiv1.PersistentVolumeClaimVolumeSource{
					ClaimName: pvc.ObjectMeta.Name,
				},
			},
		}
		woc.wf.Status.PersistentVolumeClaims[i] = vol
		woc.updated = true
	}
	return nil
}

func (woc *wfOperationCtx) deletePVCs() error {
	totalPVCs := len(woc.wf.Status.PersistentVolumeClaims)
	if totalPVCs == 0 {
		return nil
	}
	pvcClient := woc.controller.kubeclientset.CoreV1().PersistentVolumeClaims(woc.wf.ObjectMeta.Namespace)
	newPVClist := make([]apiv1.Volume, 0)
	var firstErr error
	for _, pvc := range woc.wf.Status.PersistentVolumeClaims {
		woc.log.Infof("Deleting PVC %s", pvc.PersistentVolumeClaim.ClaimName)
		err := pvcClient.Delete(context.TODO(), pvc.PersistentVolumeClaim.ClaimName, metav1.DeleteOptions{})
		if err != nil {
			if !apierr.IsNotFound(err) {
				woc.log.Errorf("Failed to delete pvc %s: %v", pvc.PersistentVolumeClaim.ClaimName, err)
				newPVClist = append(newPVClist, pvc)
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	if len(newPVClist) != totalPVCs {
		woc.log.Infof("Deleted %d/%d PVCs", totalPVCs-len(newPVClist), totalPVCs)
		woc.wf.Status.PersistentVolumeClaims = newPVClist
		woc.updated = true
	}
	return firstErr
}

func (woc *wfOperationCtx) getLastChildNode(node *wfv1.NodeStatus) (*wfv1.NodeStatus, error) {
	if len(node.Children) <= 0 {
		return nil, nil
	}

	lastChildNodeName := node.Children[len(node.Children)-1]
	lastChildNode, ok := woc.wf.Status.Nodes[lastChildNodeName]
	if !ok {
		return nil, fmt.Errorf("Failed to find node " + lastChildNodeName)
	}

	return &lastChildNode, nil
}

func (woc *wfOperationCtx) getNode(nodeName string) wfv1.NodeStatus {
	nodeID := woc.wf.NodeID(nodeName)
	node, ok := woc.wf.Status.Nodes[nodeID]
	if !ok {
		panic("Failed to find node " + nodeName)
	}

	return node
}

func (woc *wfOperationCtx) executeTemplate(templateName string, args wfv1.Arguments, nodeName string) error {
	woc.log.Debugf("Evaluating node %s: template: %s", nodeName, templateName)
	nodeID := woc.wf.NodeID(nodeName)
	node, ok := woc.wf.Status.Nodes[nodeID]
	if ok && node.Completed() {
		woc.log.Debugf("Node %s already completed", nodeName)
		return nil
	}
	tmpl := woc.wf.GetTemplate(templateName)
	if tmpl == nil {
		err := errors.Errorf(errors.CodeBadRequest, "Node %v error: template '%s' undefined", node, templateName)
		woc.markNodeError(nodeName, err)
		return err
	}

	tmpl, err := common.ProcessArgs(tmpl, args, woc.globalParams, false)
	if err != nil {
		woc.markNodeError(nodeName, err)
		return err
	}

	switch tmpl.GetType() {
	case wfv1.TemplateTypeContainer:
		if ok {
			if node.RetryStrategy != nil {
				if err = woc.processNodeRetries(&node); err != nil {
					return err
				}

				node = woc.getNode(node.Name)
				log.Infof("Node %s: Status: %s", node.Name, node.Phase)
				if node.Completed() {
					return nil
				}
				lastChildNode, err := woc.getLastChildNode(&node)
				if err != nil {
					return err
				}
				if !lastChildNode.Completed() {
					return nil
				}
			} else {
				return nil
			}
		}

		nodeToExecute := nodeName
		if tmpl.RetryStrategy != nil {
			node := woc.markNodePhase(nodeName, wfv1.NodeRunning)
			retries := wfv1.RetryStrategy{}
			node.RetryStrategy = &retries
			node.RetryStrategy.Limit = tmpl.RetryStrategy.Limit
			woc.wf.Status.Nodes[nodeID] = *node

			newContainerName := fmt.Sprintf("%s(%d)", nodeName, len(node.Children))
			woc.markNodePhase(newContainerName, wfv1.NodeRunning)
			woc.addChildNode(nodeName, newContainerName)
			nodeToExecute = newContainerName
		}

		err = woc.executeContainer(nodeToExecute, tmpl)
	case wfv1.TemplateTypeSteps:
		if !ok {
			node = *woc.markNodePhase(nodeName, wfv1.NodeRunning)
			woc.log.Infof("Initialized workflow node %v", node)
		}
		err = woc.executeSteps(nodeName, tmpl)
	case wfv1.TemplateTypeScript:
		if !ok {
			err = woc.executeScript(nodeName, tmpl)
		}
	case wfv1.TemplateTypeResource:
		if !ok {
			err = woc.executeResource(nodeName, tmpl)
		}
	default:
		err = errors.Errorf("Template '%s' missing specification", tmpl.Name)
		woc.markNodeError(nodeName, err)
	}
	if err != nil {
		return err
	}
	if time.Now().UTC().After(woc.deadline) {
		woc.log.Warnf("Deadline exceeded")
		return errors.New(errors.CodeTimeout, "Deadline exceeded")
	}
	return nil
}

// markWorkflowPhase is a convenience method to set the phase of the workflow with optional message
func (woc *wfOperationCtx) markWorkflowPhase(phase wfv1.NodePhase, markCompleted bool, message ...string) {
	if woc.wf.Status.Phase != phase {
		woc.log.Infof("Updated phase %s -> %s", woc.wf.Status.Phase, phase)
		woc.updated = true
		woc.wf.Status.Phase = phase
		if woc.wf.ObjectMeta.Labels == nil {
			woc.wf.ObjectMeta.Labels = make(map[string]string)
		}
		woc.wf.ObjectMeta.Labels[common.LabelKeyPhase] = string(phase)
	}
	if woc.wf.Status.StartedAt.IsZero() {
		woc.updated = true
		woc.wf.Status.StartedAt = metav1.Time{Time: time.Now().UTC()}
	}
	if len(message) > 0 && woc.wf.Status.Message != message[0] {
		woc.log.Infof("Updated message %s -> %s", woc.wf.Status.Message, message[0])
		woc.updated = true
		woc.wf.Status.Message = message[0]
	}

	switch phase {
	case wfv1.NodeSucceeded, wfv1.NodeFailed, wfv1.NodeError:
		if markCompleted {
			woc.log.Infof("Marking workflow completed")
			woc.wf.Status.FinishedAt = metav1.Time{Time: time.Now().UTC()}
			if woc.wf.ObjectMeta.Labels == nil {
				woc.wf.ObjectMeta.Labels = make(map[string]string)
			}
			woc.wf.ObjectMeta.Labels[common.LabelKeyCompleted] = "true"
			woc.updated = true
		}
	}
}

func (woc *wfOperationCtx) markWorkflowRunning() {
	woc.markWorkflowPhase(wfv1.NodeRunning, false)
}

func (woc *wfOperationCtx) markWorkflowSuccess() {
	woc.markWorkflowPhase(wfv1.NodeSucceeded, true)
}

func (woc *wfOperationCtx) markWorkflowFailed(message string) {
	woc.markWorkflowPhase(wfv1.NodeFailed, true, message)
}

func (woc *wfOperationCtx) markWorkflowError(err error, markCompleted bool) {
	woc.markWorkflowPhase(wfv1.NodeError, markCompleted, err.Error())
}

// markNodePhase marks a node with the given phase, creating the node if necessary and handles timestamps
func (woc *wfOperationCtx) markNodePhase(nodeName string, phase wfv1.NodePhase, message ...string) *wfv1.NodeStatus {
	nodeID := woc.wf.NodeID(nodeName)
	node, ok := woc.wf.Status.Nodes[nodeID]
	if !ok {
		node = wfv1.NodeStatus{
			ID:        nodeID,
			Name:      nodeName,
			Phase:     phase,
			StartedAt: metav1.Time{Time: time.Now().UTC()},
		}
	} else {
		node.Phase = phase
	}
	if len(message) > 0 {
		node.Message = message[0]
	}
	if node.Completed() && node.FinishedAt.IsZero() {
		node.FinishedAt = metav1.Time{Time: time.Now().UTC()}
	}
	woc.wf.Status.Nodes[nodeID] = node
	woc.updated = true
	woc.log.Debugf("Marked node %s %s", nodeName, phase)
	return &node
}

// markNodeError is a convenience method to mark a node with an error and set the message from the error
func (woc *wfOperationCtx) markNodeError(nodeName string, err error) *wfv1.NodeStatus {
	return woc.markNodePhase(nodeName, wfv1.NodeError, err.Error())
}

func (woc *wfOperationCtx) executeContainer(nodeName string, tmpl *wfv1.Template) error {
	woc.log.Debugf("Executing node %s with container template: %v\n", nodeName, tmpl)
	_, err := woc.createWorkflowPod(nodeName, *tmpl.Container, tmpl)
	if err != nil {
		woc.markNodeError(nodeName, err)
		return err
	}
	node := woc.markNodePhase(nodeName, wfv1.NodeRunning)
	woc.log.Infof("Initialized container node %v", node)
	return nil
}

// getTemplateOutputsFromScope resolves a template's outputs from the scope of the template
func getTemplateOutputsFromScope(tmpl *wfv1.Template, scope *wfScope) (*wfv1.Outputs, error) {
	if !tmpl.Outputs.HasOutputs() {
		return nil, nil
	}
	var outputs wfv1.Outputs
	if len(tmpl.Outputs.Parameters) > 0 {
		outputs.Parameters = make([]wfv1.Parameter, 0)
		for _, param := range tmpl.Outputs.Parameters {
			val, err := scope.resolveParameter(param.ValueFrom.Parameter)
			if err != nil {
				return nil, err
			}
			param.Value = &val
			param.ValueFrom = nil
			outputs.Parameters = append(outputs.Parameters, param)
		}
	}
	if len(tmpl.Outputs.Artifacts) > 0 {
		outputs.Artifacts = make([]wfv1.Artifact, 0)
		for _, art := range tmpl.Outputs.Artifacts {
			resolvedArt, err := scope.resolveArtifact(art.From)
			if err != nil {
				return nil, err
			}
			resolvedArt.Name = art.Name
			outputs.Artifacts = append(outputs.Artifacts, *resolvedArt)
		}
	}
	return &outputs, nil
}

func (woc *wfOperationCtx) executeScript(nodeName string, tmpl *wfv1.Template) error {
	mainCtr := apiv1.Container{
		Image:   tmpl.Script.Image,
		Command: tmpl.Script.Command,
		Args:    []string{common.ExecutorScriptSourcePath},
	}
	_, err := woc.createWorkflowPod(nodeName, mainCtr, tmpl)
	if err != nil {
		woc.markNodeError(nodeName, err)
		return err
	}
	node := woc.markNodePhase(nodeName, wfv1.NodeRunning)
	woc.log.Infof("Initialized script node %v", node)
	return nil
}

func (wfs *wfScope) addParamToScope(key, val string) {
	wfs.scope[key] = val
}

func (wfs *wfScope) addArtifactToScope(key string, artifact wfv1.Artifact) {
	wfs.scope[key] = artifact
}

func (wfs *wfScope) resolveVar(v string) (interface{}, error) {
	v = strings.TrimPrefix(v, "{{")
	v = strings.TrimSuffix(v, "}}")
	if strings.HasPrefix(v, "steps.") {
		val, ok := wfs.scope[v]
		if !ok {
			return nil, errors.Errorf(errors.CodeBadRequest, "Unable to resolve: {{%s}}", v)
		}
		return val, nil
	}
	parts := strings.Split(v, ".")
	art := wfs.tmpl.Inputs.GetArtifactByName(parts[2])
	if art != nil {
		return *art, nil
	}
	return nil, errors.Errorf(errors.CodeBadRequest, "Unable to resolve input artifact: {{%s}}", v)
}

func (wfs *wfScope) resolveParameter(v string) (string, error) {
	val, err := wfs.resolveVar(v)
	if err != nil {
		return "", err
	}
	valStr, ok := val.(string)
	if !ok {
		return "", errors.Errorf(errors.CodeBadRequest, "Variable {{%s}} is not a string", v)
	}
	return valStr, nil
}

func (wfs *wfScope) resolveArtifact(v string) (*wfv1.Artifact, error) {
	val, err := wfs.resolveVar(v)
	if err != nil {
		return nil, err
	}
	valArt, ok := val.(wfv1.Artifact)
	if !ok {
		return nil, errors.Errorf(errors.CodeBadRequest, "Variable {{%s}} is not an artifact", v)
	}
	return &valArt, nil
}

// addChildNode adds a nodeID as a child to a parent
func (woc *wfOperationCtx) addChildNode(parent string, child string) {
	parentID := woc.wf.NodeID(parent)
	childID := woc.wf.NodeID(child)
	node, ok := woc.wf.Status.Nodes[parentID]
	if !ok {
		panic(fmt.Sprintf("parent node %s not initialized", parent))
	}
	if node.Children == nil {
		node.Children = make([]string, 0)
	}
	for _, nodeID := range node.Children {
		if childID == nodeID {
			return
		}
	}
	node.Children = append(node.Children, childID)
	woc.wf.Status.Nodes[parentID] = node
	woc.updated = true
}

// executeResource is runs a kubectl command against a manifest
func (woc *wfOperationCtx) executeResource(nodeName string, tmpl *wfv1.Template) error {
	mainCtr := apiv1.Container{
		Image:   woc.controller.Config.ExecutorImage,
		Command: []string{"taskerexec"},
		Args:    []string{"resource", tmpl.Resource.Action},
		VolumeMounts: []apiv1.VolumeMount{
			volumeMountPodMetadata,
		},
		Env: execEnvVars,
	}
	_, err := woc.createWorkflowPod(nodeName, mainCtr, tmpl)
	if err != nil {
		woc.markNodeError(nodeName, err)
		return err
	}
	node := woc.markNodePhase(nodeName, wfv1.NodeRunning)
	woc.log.Infof("Initialized resource node %v", node)
	return nil
}

func processItem(fstTmpl *fasttemplate.Template, name string, index int, item wfv1.Item, obj interface{}) (string, error) {
	replaceMap := make(map[string]string)
	var newName string

	switch item.Type {
	case wfv1.String, wfv1.Number, wfv1.Bool:
		replaceMap["item"] = fmt.Sprintf("%v", item)
		newName = fmt.Sprintf("%s(%d:%v)", name, index, item)
	case wfv1.Map:
		vals := make([]string, 0)
		for itemKey, itemVal := range item.MapVal {
			replaceMap[fmt.Sprintf("item.%s", itemKey)] = fmt.Sprintf("%v", itemVal)
			vals = append(vals, fmt.Sprintf("%s:%s", itemKey, itemVal))

		}
		jsonByteVal, err := json.Marshal(item.MapVal)
		if err != nil {
			return "", errors.InternalWrapError(err)
		}
		replaceMap["item"] = string(jsonByteVal)

		sort.Strings(vals)
		newName = fmt.Sprintf("%s(%d:%v)", name, index, strings.Join(vals, ","))
	case wfv1.List:
		byteVal, err := json.Marshal(item.ListVal)
		if err != nil {
			return "", errors.InternalWrapError(err)
		}
		replaceMap["item"] = string(byteVal)
		newName = fmt.Sprintf("%s(%d:%v)", name, index, item.ListVal)
	default:
		return "", errors.Errorf(errors.CodeBadRequest, "withItems[%d] expected string, number, list, or map. received: %v", index, item)
	}
	newStepStr, err := common.Replace(fstTmpl, replaceMap, false, "")
	if err != nil {
		return "", err
	}
	err = json.Unmarshal([]byte(newStepStr), &obj)
	if err != nil {
		return "", errors.InternalWrapError(err)
	}
	return newName, nil
}
