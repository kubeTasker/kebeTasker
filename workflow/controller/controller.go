package controller

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ghodss/yaml"
	"github.com/kubeTasker/kubeTasker/errors"
	wfv1 "github.com/kubeTasker/kubeTasker/pkg/apis/workflow/v1alpha1"
	wfclientset "github.com/kubeTasker/kubeTasker/pkg/client/clientset/versioned"
	wfinformers "github.com/kubeTasker/kubeTasker/pkg/client/informers/externalversions"
	"github.com/kubeTasker/kubeTasker/workflow/common"
	log "github.com/sirupsen/logrus"
	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type WorkflowController struct {
	ConfigMap   string
	ConfigMapNS string
	Config      WorkflowControllerConfig

	restConfig    *rest.Config
	kubeclientset kubernetes.Interface
	wfclientset   wfclientset.Interface

	wfInformer    cache.SharedIndexInformer
	podInformer   cache.SharedIndexInformer
	wfQueue       workqueue.RateLimitingInterface
	podQueue      workqueue.RateLimitingInterface
	completedPods chan string
}

// WorkflowControllerConfig contain the configuration settings for the workflow controller
type WorkflowControllerConfig struct {
	ExecutorImage string `json:"executorImage,omitempty"`

	ExecutorResources *apiv1.ResourceRequirements `json:"executorResources,omitempty"`

	ArtifactRepository ArtifactRepository `json:"artifactRepository,omitempty"`

	Namespace string `json:"namespace,omitempty"`

	InstanceID string `json:"instanceID,omitempty"`

	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

const (
	workflowResyncPeriod = 20 * time.Minute
	podResyncPeriod      = 30 * time.Minute
)

// ArtifactRepository represents a artifact repository in which a controller will store its artifacts
type ArtifactRepository struct {
	S3          *S3ArtifactRepository          `json:"s3,omitempty"`
	Artifactory *ArtifactoryArtifactRepository `json:"artifactory,omitempty"`
}

type S3ArtifactRepository struct {
	wfv1.S3Bucket `json:",inline"`

	KeyPrefix string `json:"keyPrefix,omitempty"`
}

type ArtifactoryArtifactRepository struct {
	wfv1.ArtifactoryAuth `json:",inline"`
	RepoURL              string `json:"repoURL,omitempty"`
}

// NewWorkflowController instantiates a new WorkflowController
func NewWorkflowController(restConfig *rest.Config, kubeclientset kubernetes.Interface, wfclientset wfclientset.Interface, configMap string) *WorkflowController {
	wfc := WorkflowController{
		restConfig:    restConfig,
		kubeclientset: kubeclientset,
		wfclientset:   wfclientset,
		ConfigMap:     configMap,
		wfQueue:       workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		podQueue:      workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()),
		completedPods: make(chan string, 512),
	}
	return &wfc
}

// Run starts an Workflow resource controller
func (wfc *WorkflowController) Run(ctx context.Context, wfWorkers, podWorkers int) {
	defer wfc.wfQueue.ShutDown()
	defer wfc.podQueue.ShutDown()

	log.Info("Watch Workflow controller config map updates")
	_, err := wfc.watchControllerConfigMap(ctx)
	if err != nil {
		log.Errorf("Failed to register watch for controller config map: %v", err)
		return
	}

	wfc.wfInformer = wfc.newWorkflowInformer()
	wfc.podInformer = wfc.newPodInformer()
	go wfc.wfInformer.Run(ctx.Done())
	go wfc.podInformer.Run(ctx.Done())
	go wfc.podLabeler(ctx.Done())

	for _, informer := range []cache.SharedIndexInformer{wfc.wfInformer, wfc.podInformer} {
		if !cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			log.Error("Timed out waiting for caches to sync")
			return
		}
	}

	for i := 0; i < wfWorkers; i++ {
		go wait.Until(wfc.runWorker, time.Second, ctx.Done())
	}
	for i := 0; i < podWorkers; i++ {
		go wait.Until(wfc.podWorker, time.Second, ctx.Done())
	}
	<-ctx.Done()
}

// podLabeler will label all pods on the controllers completedPod channel as completed
func (wfc *WorkflowController) podLabeler(stopCh <-chan struct{}) {
	for {
		select {
		case <-stopCh:
			return
		case pod := <-wfc.completedPods:
			parts := strings.Split(pod, "/")
			if len(parts) != 2 {
				log.Warnf("Unexpected item on completedpod  channel: %s", pod)
				continue
			}
			namespace := parts[0]
			podName := parts[1]
			err := common.AddPodLabel(wfc.kubeclientset, podName, namespace, common.LabelKeyCompleted, "true")
			if err != nil {
				log.Errorf("Failed to label pod %s/%s completed: %+v", namespace, podName, err)
			} else {
				log.Infof("Labeled pod %s/%s completed", namespace, podName)
			}
		}
	}
}

func (wfc *WorkflowController) runWorker() {
	for wfc.processNextItem() {
	}
}

// processNextItem is the worker logic for handling workflow updates
func (wfc *WorkflowController) processNextItem() bool {
	key, quit := wfc.wfQueue.Get()
	if quit {
		return false
	}
	defer wfc.wfQueue.Done(key)

	obj, exists, err := wfc.wfInformer.GetIndexer().GetByKey(key.(string))
	if err != nil {
		log.Errorf("Failed to get workflow '%s' from informer index: %+v", key, err)
		return true
	}
	if !exists {
		return true
	}
	wf, ok := obj.(*wfv1.Workflow)
	if !ok {
		log.Warnf("Key '%s' in index is not a workflow", key)
		return true
	}

	if wf.ObjectMeta.Labels[common.LabelKeyCompleted] == "true" {
		return true
	}
	woc := newWorkflowOperationCtx(wf, wfc)
	woc.operate()
	return true
}

func (wfc *WorkflowController) podWorker() {
	for wfc.processNextPodItem() {
	}
}

// processNextPodItem is the worker logic for handling pod updates.
func (wfc *WorkflowController) processNextPodItem() bool {
	key, quit := wfc.podQueue.Get()
	if quit {
		return false
	}
	defer wfc.podQueue.Done(key)

	obj, exists, err := wfc.podInformer.GetIndexer().GetByKey(key.(string))
	if err != nil {
		log.Errorf("Failed to get pod '%s' from informer index: %+v", key, err)
		return true
	}
	if !exists {
		return true
	}
	pod, ok := obj.(*apiv1.Pod)
	if !ok {
		log.Warnf("Key '%s' in index is not a pod", key)
		return true
	}
	if pod.Labels == nil {
		log.Warnf("Pod '%s' did not have labels", key)
		return true
	}
	workflowName, ok := pod.Labels[common.LabelKeyWorkflow]
	if !ok {
		log.Warnf("watch returned pod unrelated to any workflow: %s", pod.ObjectMeta.Name)
		return true
	}
	wfc.wfQueue.Add(pod.ObjectMeta.Namespace + "/" + workflowName)
	return true
}

// ResyncConfig reloads the controller config from the configmap
func (wfc *WorkflowController) ResyncConfig() error {
	namespace, _ := os.LookupEnv(common.EnvVarNamespace)
	if namespace == "" {
		namespace = common.DefaultControllerNamespace
	}
	cmClient := wfc.kubeclientset.CoreV1().ConfigMaps(namespace)
	cm, err := cmClient.Get(context.TODO(), wfc.ConfigMap, metav1.GetOptions{})
	if err != nil {
		return errors.InternalWrapError(err)
	}
	wfc.ConfigMapNS = cm.Namespace
	return wfc.updateConfig(cm)
}

func (wfc *WorkflowController) updateConfig(cm *apiv1.ConfigMap) error {
	configStr, ok := cm.Data[common.WorkflowControllerConfigMapKey]
	if !ok {
		return errors.Errorf(errors.CodeBadRequest, "ConfigMap '%s' does not have key '%s'", wfc.ConfigMap, common.WorkflowControllerConfigMapKey)
	}
	var config WorkflowControllerConfig
	err := yaml.Unmarshal([]byte(configStr), &config)
	if err != nil {
		return errors.InternalWrapError(err)
	}
	log.Printf("workflow controller configuration from %s:\n%s", wfc.ConfigMap, configStr)
	if config.ExecutorImage == "" {
		return errors.Errorf(errors.CodeBadRequest, "ConfigMap '%s' does not have executorImage", wfc.ConfigMap)
	}
	wfc.Config = config
	return nil
}

// instanceIDRequirement returns the label requirement to filter against a controller instance (or not)
func (wfc *WorkflowController) instanceIDRequirement() labels.Requirement {
	var instanceIDReq *labels.Requirement
	var err error
	if wfc.Config.InstanceID != "" {
		instanceIDReq, err = labels.NewRequirement(common.LabelKeyControllerInstanceID, selection.Equals, []string{wfc.Config.InstanceID})
	} else {
		instanceIDReq, err = labels.NewRequirement(common.LabelKeyControllerInstanceID, selection.DoesNotExist, nil)
	}
	if err != nil {
		panic(err)
	}
	return *instanceIDReq
}

func (wfc *WorkflowController) tweakWorkflowlist(options *metav1.ListOptions) {
	options.FieldSelector = fields.Everything().String()

	incompleteReq, err := labels.NewRequirement(common.LabelKeyCompleted, selection.NotIn, []string{"true"})
	if err != nil {
		panic(err)
	}
	labelSelector := labels.NewSelector().
		Add(*incompleteReq).
		Add(wfc.instanceIDRequirement())
	options.LabelSelector = labelSelector.String()
}

func (wfc *WorkflowController) newWorkflowInformer() cache.SharedIndexInformer {
	wfInformerFactory := wfinformers.NewFilteredSharedInformerFactory(
		wfc.wfclientset,
		workflowResyncPeriod,
		wfc.Config.Namespace,
		wfc.tweakWorkflowlist,
	)
	informer := wfInformerFactory.Kubetasker().V1alpha1().Workflows().Informer()
	_, err := informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					wfc.wfQueue.Add(key)
				}
			},
			UpdateFunc: func(old, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					wfc.wfQueue.Add(key)
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					wfc.wfQueue.Add(key)
				}
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	return informer
}

func (wfc *WorkflowController) watchControllerConfigMap(ctx context.Context) (cache.Controller, error) {
	source := wfc.newControllerConfigMapWatch()
	_, controller := cache.NewInformer(
		source,
		&apiv1.ConfigMap{},
		0,
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				if cm, ok := obj.(*apiv1.ConfigMap); ok {
					log.Infof("Detected ConfigMap update. Updating the controller config.")
					err := wfc.updateConfig(cm)
					if err != nil {
						log.Errorf("Update of config failed due to: %v", err)
					}
				}
			},
			UpdateFunc: func(old, new interface{}) {
				if newCm, ok := new.(*apiv1.ConfigMap); ok {
					log.Infof("Detected ConfigMap update. Updating the controller config.")
					err := wfc.updateConfig(newCm)
					if err != nil {
						log.Errorf("Update of config failed due to: %v", err)
					}
				}
			},
		})

	go controller.Run(ctx.Done())
	return controller, nil
}

func (wfc *WorkflowController) newControllerConfigMapWatch() *cache.ListWatch {
	c := wfc.kubeclientset.CoreV1().RESTClient()
	resource := "configmaps"
	name := wfc.ConfigMap
	namespace := wfc.ConfigMapNS
	fieldSelector := fields.ParseSelectorOrDie(fmt.Sprintf("metadata.name=%s", name))

	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		options.FieldSelector = fieldSelector.String()
		req := c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec)
		return req.Do(context.TODO()).Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		options.FieldSelector = fieldSelector.String()
		req := c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec)
		return req.Watch(context.TODO())
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

func (wfc *WorkflowController) newWorkflowPodWatch() *cache.ListWatch {
	c := wfc.kubeclientset.CoreV1().RESTClient()
	resource := "pods"
	namespace := wfc.Config.Namespace
	fieldSelector := fields.ParseSelectorOrDie("status.phase!=Pending")
	// completed=false
	incompleteReq, _ := labels.NewRequirement(common.LabelKeyCompleted, selection.Equals, []string{"false"})
	labelSelector := labels.NewSelector().
		Add(*incompleteReq).
		Add(wfc.instanceIDRequirement())

	listFunc := func(options metav1.ListOptions) (runtime.Object, error) {
		options.FieldSelector = fieldSelector.String()
		options.LabelSelector = labelSelector.String()
		req := c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec)
		return req.Do(context.TODO()).Get()
	}
	watchFunc := func(options metav1.ListOptions) (watch.Interface, error) {
		options.Watch = true
		options.FieldSelector = fieldSelector.String()
		options.LabelSelector = labelSelector.String()
		req := c.Get().
			Namespace(namespace).
			Resource(resource).
			VersionedParams(&options, metav1.ParameterCodec)
		return req.Watch(context.TODO())
	}
	return &cache.ListWatch{ListFunc: listFunc, WatchFunc: watchFunc}
}

func (wfc *WorkflowController) newPodInformer() cache.SharedIndexInformer {
	source := wfc.newWorkflowPodWatch()
	informer := cache.NewSharedIndexInformer(source, &apiv1.Pod{}, podResyncPeriod, cache.Indexers{})
	_, err := informer.AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(obj)
				if err == nil {
					wfc.podQueue.Add(key)
				}
			},
			UpdateFunc: func(old, new interface{}) {
				key, err := cache.MetaNamespaceKeyFunc(new)
				if err == nil {
					wfc.podQueue.Add(key)
				}
			},
			DeleteFunc: func(obj interface{}) {
				key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
				if err == nil {
					wfc.podQueue.Add(key)
				}
			},
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	return informer
}
