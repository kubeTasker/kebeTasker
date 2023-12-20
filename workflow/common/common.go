package common

import (
	"time"

	"github.com/kubeTasker/kubeTasker/pkg/apis/workflow"
)

const (
	DefaultControllerDeploymentName = "workflow-controller"
	DefaultControllerNamespace      = "kube-system"

	WorkflowControllerConfigMapKey = "config"

	MainContainerName = "main"
	InitContainerName = "init"
	WaitContainerName = "wait"

	PodMetadataVolumeName = "podmetadata"

	PodMetadataAnnotationsVolumePath = "annotations"
	PodMetadataMountPath             = "/kubetasker/" + PodMetadataVolumeName
	PodMetadataAnnotationsPath       = PodMetadataMountPath + "/" + PodMetadataAnnotationsVolumePath

	DockerLibVolumeName  = "docker-lib"
	DockerLibHostPath    = "/var/lib/docker"
	DockerSockVolumeName = "docker-sock"

	AnnotationKeyNodeName         = workflow.FullName + "/node-name"
	AnnotationKeyNodeMessage      = workflow.FullName + "/node-message"
	AnnotationKeyTemplate         = workflow.FullName + "/template"
	AnnotationKeyOutputs          = workflow.FullName + "/outputs"
	AnnotationKeyExecutionControl = workflow.FullName + "/execution"

	LabelKeyControllerInstanceID = workflow.FullName + "/controller-instanceid"
	LabelKeyCompleted            = workflow.FullName + "/completed"
	LabelKeyWorkflow             = workflow.FullName + "/workflow"
	LabelKeyPhase                = workflow.FullName + "/phase"

	ExecutorArtifactBaseDir = "/kubetasker/inputs/artifacts"

	InitContainerMainFilesystemDir = "/mainctrfs"

	ExecutorStagingEmptyDir      = "/kubetasker/staging"
	ExecutorScriptSourcePath     = "/kubetasker/staging/script"
	ExecutorResourceManifestPath = "/tmp/manifest.yaml"

	EnvVarPodIP       = "KUBETASKER_POD_IP"
	EnvVarPodName     = "KUBETASKER_POD_NAME"
	EnvVarNamespace   = "KUBETASKER_NAMESPACE"
	EnvVarTaskerTrace = "Tasker_TRACE"

	GlobalVarWorkflowName   = "workflow.name"
	GlobalVarWorkflowUID    = "workflow.uid"
	GlobalVarWorkflowStatus = "workflow.status"
)

// ExecutionControl contains execution control parameters for executor to decide how to execute the container
type ExecutionControl struct {
	Deadline *time.Time `json:"deadline,omitempty"`
}
