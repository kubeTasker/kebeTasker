package v1alpha1

import (
	"encoding/json"
	"fmt"
	"hash/fnv"

	apiv1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TemplateType is the type of a template
type TemplateType string

// Possible template types
const (
	TemplateTypeContainer TemplateType = "Container"
	TemplateTypeSteps     TemplateType = "Steps"
	TemplateTypeScript    TemplateType = "Script"
	TemplateTypeResource  TemplateType = "Resource"
)

// NodePhase is a label for the condition of a node at the current time.
type NodePhase string

// Workflow and node statuses
const (
	NodeRunning   NodePhase = "Running"
	NodeSucceeded NodePhase = "Succeeded"
	NodeSkipped   NodePhase = "Skipped"
	NodeFailed    NodePhase = "Failed"
	NodeError     NodePhase = "Error"
)

// Workflow is the definition of a workflow resource
// +genclient
// +genclient:noStatus
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata" protobuf:"bytes,1,opt,name=metadata"`
	Spec              WorkflowSpec   `json:"spec" protobuf:"bytes,2,opt,name=spec"`
	Status            WorkflowStatus `json:"status" protobuf:"bytes,3,opt,name=status"`
}

// WorkflowList is list of Workflow resources
// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata" protobuf:"bytes,1,opt,name=metadata"`
	Items           []Workflow `json:"items" protobuf:"bytes,2,opt,name=items"`
}

// WorkflowSpec is the specification of a Workflow.
type WorkflowSpec struct {
	Templates []Template `json:"templates" protobuf:"bytes,1,opt,name=templates"`

	Entrypoint string `json:"entrypoint" protobuf:"bytes,2,opt,name=entrypoint"`

	Arguments Arguments `json:"arguments,omitempty" protobuf:"bytes,3,opt,name=arguments"`

	ServiceAccountName string `json:"serviceAccountName,omitempty" protobuf:"bytes,4,opt,name=serviceAccountName"`

	Volumes []apiv1.Volume `json:"volumes,omitempty" protobuf:"bytes,5,opt,name=volumes"`

	VolumeClaimTemplates []apiv1.PersistentVolumeClaim `json:"volumeClaimTemplates,omitempty" protobuf:"bytes,6,opt,name=volumeClaimTemplates"`

	NodeSelector map[string]string `json:"nodeSelector,omitempty" protobuf:"bytes,7,opt,name=nodeSelector"`

	Affinity *apiv1.Affinity `json:"affinity,omitempty" protobuf:"bytes,8,opt,name=affinity"`

	ImagePullSecrets []apiv1.LocalObjectReference `json:"imagePullSecrets,omitempty" protobuf:"bytes,9,opt,name=imagePullSecrets"`

	OnExit string `json:"onExit,omitempty" protobuf:"bytes,10,opt,name=onExit"`
}

// Template is a reusable and composable unit of execution in a workflow
type Template struct {
	Name string `json:"name" protobuf:"bytes,1,opt,name=name"`

	Inputs Inputs `json:"inputs,omitempty" protobuf:"bytes,2,opt,name=inputs"`

	Outputs Outputs `json:"outputs,omitempty" protobuf:"bytes,3,opt,name=outputs"`

	NodeSelector map[string]string `json:"nodeSelector,omitempty" protobuf:"bytes,4,opt,name=nodeSelector"`

	Affinity *apiv1.Affinity `json:"affinity,omitempty" protobuf:"bytes,5,opt,name=affinity"`

	Daemon *bool `json:"daemon,omitempty" protobuf:"bytes,6,opt,name=daemon"`

	Steps [][]WorkflowStep `json:"steps,omitempty" protobuf:"bytes,7,opt,name=steps"`

	Container *apiv1.Container `json:"container,omitempty" protobuf:"bytes,8,opt,name=container"`

	Script *Script `json:"script,omitempty" protobuf:"bytes,9,opt,name=script"`

	Resource *ResourceTemplate `json:"resource,omitempty" protobuf:"bytes,10,opt,name=resource"`

	Sidecars []Sidecar `json:"sidecars,omitempty" protobuf:"bytes,11,opt,name=sidecars"`

	ArchiveLocation *ArtifactLocation `json:"archiveLocation,omitempty" protobuf:"bytes,12,opt,name=archieveLocation"`

	ActiveDeadlineSeconds *int64 `json:"activeDeadlineSeconds,omitempty" protobuf:"bytes,13,opt,name=activeDeadlineSeconds"`

	RetryStrategy *RetryStrategy `json:"retryStrategy,omitempty" protobuf:"bytes,14,opt,name=retryStrategy"`
}

// Inputs are the mechanism for passing parameters, artifacts, volumes from one template to another
type Inputs struct {
	Parameters []Parameter `json:"parameters,omitempty" protobuf:"bytes,1,opt,name=parameters"`

	Artifacts []Artifact `json:"artifacts,omitempty" protobuf:"bytes,2,opt,name=artifacts"`
}

// Parameter indicate a passed string parameter to a service template with an optional default value
type Parameter struct {
	Name string `json:"name" protobuf:"bytes,1,opt,name=name"`

	Default *string `json:"default,omitempty" protobuf:"bytes,2,opt,name=default"`

	Value *string `json:"value,omitempty" protobuf:"bytes,3,opt,name=value"`

	ValueFrom *ValueFrom `json:"valueFrom,omitempty" protobuf:"bytes,4,opt,name=valueFrom"`
}

// ValueFrom describes a location in which to obtain the value to a parameter
type ValueFrom struct {
	Path string `json:"path,omitempty" protobuf:"bytes,1,opt,name=path"`

	JSONPath string `json:"jsonPath,omitempty" protobuf:"bytes,2,opt,name=jsonPath"`

	JQFilter string `json:"jqFilter,omitempty" protobuf:"bytes,3,opt,name=jqFilter"`

	Parameter string `json:"parameter,omitempty" protobuf:"bytes,4,opt,name=parameter"`
}

// Artifact indicates an artifact to place at a specified path
type Artifact struct {
	Name string `json:"name" protobuf:"bytes,1,opt,name=name"`

	Path string `json:"path,omitempty" protobuf:"bytes,2,opt,name=path"`

	Mode *int32 `json:"mode,omitempty" protobuf:"bytes,3,opt,name=mode"`

	From string `json:"from,omitempty" protobuf:"bytes,4,opt,name=from"`

	ArtifactLocation `json:",inline" protobuf:"bytes,5,opt,name=artifactLocation"`
}

// ArtifactLocation describes a location for a single or multiple artifacts.
type ArtifactLocation struct {
	S3 *S3Artifact `json:"s3,omitempty" protobuf:"bytes,1,opt,name=s3"`

	Git *GitArtifact `json:"git,omitempty" protobuf:"bytes,2,opt,name=git"`

	HTTP *HTTPArtifact `json:"http,omitempty" protobuf:"bytes,3,opt,name=http"`

	Artifactory *ArtifactoryArtifact `json:"artifactory,omitempty" protobuf:"bytes,4,opt,name=artifactory"`

	Raw *RawArtifact `json:"raw,omitempty" protobuf:"bytes,5,opt,name=raw"`
}

// Outputs hold parameters, artifacts, and results from a step
type Outputs struct {
	Parameters []Parameter `json:"parameters,omitempty" protobuf:"bytes,1,opt,name=parameters"`

	Artifacts []Artifact `json:"artifacts,omitempty" protobuf:"bytes,2,opt,name=artifacts"`

	Result *string `json:"result,omitempty" protobuf:"bytes,3,opt,name=result"`
}

// WorkflowStep is a reference to a template to execute in a series of step
type WorkflowStep struct {
	Name string `json:"name,omitempty" protobuf:"bytes,1,opt,name=name"`

	Template string `json:"template,omitempty" protobuf:"bytes,2,opt,name=template"`

	Arguments Arguments `json:"arguments,omitempty" protobuf:"bytes,3,opt,name=arguments"`

	// +k8s:openapi-gen=false
	WithItems []Item `json:"withItems,omitempty" protobuf:"bytes,4,opt,name=withItems"`

	WithParam string `json:"withParam,omitempty" protobuf:"bytes,5,opt,name=withParam"`

	When string `json:"when,omitempty" protobuf:"bytes,6,opt,name=when"`
}

// Arguments to a template
type Arguments struct {
	Parameters []Parameter `json:"parameters,omitempty" protobuf:"bytes,1,opt,name=parameters"`

	Artifacts []Artifact `json:"artifacts,omitempty" protobuf:"bytes,2,opt,name=artifacts"`
}

// Sidecar is a container which runs alongside the main container
type Sidecar struct {
	apiv1.Container `json:",inline" protobuf:"bytes,1,opt,name=container"`

	SidecarOptions `json:",inline" protobuf:"bytes,2,opt,name=sizecarOptions"`
}

// SidecarOptions provide a way to customize the behavior of a sidecar and how it
// affects the main container.
type SidecarOptions struct {

	MirrorVolumeMounts *bool `json:"mirrorVolumeMounts,omitempty" protobuf:"bytes,1,opt,name=mirrorVolumeMounts"`

}

// WorkflowStatus contains overall status information about a workflow
// +k8s:openapi-gen=false
type WorkflowStatus struct {
	Phase NodePhase `json:"phase" protobuf:"bytes,1,opt,name=phase"`

	StartedAt metav1.Time `json:"startedAt,omitempty" protobuf:"bytes,2,opt,name=startedAt"`

	FinishedAt metav1.Time `json:"finishedAt,omitempty" protobuf:"bytes,3,opt,name=finishedAt"`

	Message string `json:"message,omitempty" protobuf:"bytes,4,opt,name=message"`

	Nodes map[string]NodeStatus `json:"nodes" protobuf:"bytes,5,opt,name=nodes"`

	PersistentVolumeClaims []apiv1.Volume `json:"persistentVolumeClaims,omitempty" protobuf:"bytes,6,opt,name=persistentVolumeClaims"`
}

// GetNodesWithRetries returns a list of nodes that have retries.
func (wfs *WorkflowStatus) GetNodesWithRetries() []NodeStatus {
	var nodesWithRetries []NodeStatus
	for _, node := range wfs.Nodes {
		if node.RetryStrategy != nil {
			nodesWithRetries = append(nodesWithRetries, node)
		}
	}
	return nodesWithRetries
}

// RetryStrategy provides controls on how to retry a workflow step
type RetryStrategy struct {
	Limit *int32 `json:"limit,omitempty" protobuf:"varint,1,opt,name=limit"`
}

// NodeStatus contains status information about an individual node in the workflow
// +k8s:openapi-gen=false
type NodeStatus struct {
	ID string `json:"id" protobuf:"bytes,1,opt,name=id"`

	Name string `json:"name" protobuf:"bytes,2,opt,name=name"`

	Phase NodePhase `json:"phase" protobuf:"bytes,3,opt,name=phase"`

	Message string `json:"message,omitempty" protobuf:"bytes,4,opt,name=message"`

	StartedAt metav1.Time `json:"startedAt,omitempty" protobuf:"bytes,5,opt,name=startedAt"`

	FinishedAt metav1.Time `json:"finishedAt,omitempty" protobuf:"bytes,6,opt,name=finishedAt"`

	PodIP string `json:"podIP,omitempty" protobuf:"bytes,7,opt,name=podIP"`

	Daemoned *bool `json:"daemoned,omitempty" protobuf:"bytes,8,opt,name=daemoned"`

	RetryStrategy *RetryStrategy `json:"retryStrategy,omitempty" protobuf:"bytes,9,opt,name=retryStrategy"`

	Outputs *Outputs `json:"outputs,omitempty" protobuf:"bytes,10,opt,name=outputs"`

	Children []string `json:"children,omitempty" protobuf:"bytes,11,opt,name=children"`
}

func (n NodeStatus) String() string {
	return fmt.Sprintf("%s (%s)", n.Name, n.ID)
}

// Completed returns whether or not the node has completed execution
func (n NodeStatus) Completed() bool {
	return n.Phase == NodeSucceeded ||
		n.Phase == NodeFailed ||
		n.Phase == NodeError ||
		n.Phase == NodeSkipped
}

// IsDaemoned returns whether or not the node is deamoned
func (n NodeStatus) IsDaemoned() bool {
	if n.Daemoned == nil || !*n.Daemoned {
		return false
	}
	return true
}

// Successful returns whether or not this node completed successfully
func (n NodeStatus) Successful() bool {
	return n.Phase == NodeSucceeded || n.Phase == NodeSkipped
}

// CanRetry returns whether the node should be retried or not.
func (n NodeStatus) CanRetry() bool {
	return n.Completed() && !n.Successful()
}

// S3Bucket contains the access information required for interfacing with an S3 bucket
type S3Bucket struct {
	Endpoint string `json:"endpoint" protobuf:"bytes,1,opt,name=endpoint"`

	Bucket string `json:"bucket" protobuf:"bytes,2,opt,name=bucket"`

	Region string `json:"region,omitempty" protobuf:"bytes,3,opt,name=region"`

	Insecure *bool `json:"insecure,omitempty" protobuf:"varint,4,opt,name=insecure"`

	AccessKeySecret apiv1.SecretKeySelector `json:"accessKeySecret" protobuf:"bytes,5,opt,name=accessKeySecret"`

	SecretKeySecret apiv1.SecretKeySelector `json:"secretKeySecret" protobuf:"bytes,6,opt,name=secretKeySecret"`
}

// S3Artifact is the location of an S3 artifact
type S3Artifact struct {
	S3Bucket `json:",inline" protobuf:"bytes,1,opt,name=s3Bucket"`

	Key string `json:"key" protobuf:"bytes,2,opt,name=key"`
}

// GitArtifact is the location of an git artifact
type GitArtifact struct {
	Repo string `json:"repo" protobuf:"bytes,1,opt,name=repo"`

	Revision string `json:"revision,omitempty" protobuf:"bytes,2,opt,name=revision"`

	UsernameSecret *apiv1.SecretKeySelector `json:"usernameSecret,omitempty" protobuf:"bytes,3,opt,name=usernameSecret"`

	PasswordSecret *apiv1.SecretKeySelector `json:"passwordSecret,omitempty" protobuf:"bytes,4,opt,name=passwordSecret"`
}

// ArtifactoryAuth describes the secret selectors required for authenticating to artifactory
type ArtifactoryAuth struct {
	UsernameSecret *apiv1.SecretKeySelector `json:"usernameSecret,omitempty" protobuf:"bytes,1,opt,name=usernameSecret"`

	PasswordSecret *apiv1.SecretKeySelector `json:"passwordSecret,omitempty" protobuf:"bytes,2,opt,name=passwordSecret"`
}

// ArtifactoryArtifact is the location of an artifactory artifact
type ArtifactoryArtifact struct {
	URL             string `json:"url" protobuf:"bytes,1,opt,name=url"`
	ArtifactoryAuth `json:",inline" protobuf:"bytes,2,opt,name=artifactoryAuth"`
}

// RawArtifact allows raw string content to be placed as an artifact in a container
type RawArtifact struct {
	Data string `json:"data" protobuf:"bytes,1,opt,name=data"`
}

// HTTPArtifact allows an file served on HTTP to be placed as an input artifact in a container
type HTTPArtifact struct {
	URL string `json:"url" protobuf:"bytes,1,opt,name=url"`
}

// Script is a template subtype to enable scripting through code steps
type Script struct {
	Image string `json:"image" protobuf:"bytes,1,opt,name=image"`

	Command []string `json:"command" protobuf:"bytes,2,opt,name=command"`

	Source string `json:"source" protobuf:"bytes,3,opt,name=source"`
}

// ResourceTemplate is a template subtype to manipulate kubernetes resources
type ResourceTemplate struct {
	Action string `json:"action" protobuf:"bytes,1,opt,name=action"`

	Manifest string `json:"manifest" protobuf:"bytes,2,opt,name=manifest"`

	SuccessCondition string `json:"successCondition,omitempty" protobuf:"bytes,3,opt,name=successCondition"`

	FailureCondition string `json:"failureCondition,omitempty" protobuf:"bytes,4,opt,name=failureCondition"`
}

// GetType returns the type of this template
func (tmpl *Template) GetType() TemplateType {
	if tmpl.Container != nil {
		return TemplateTypeContainer
	}
	if tmpl.Steps != nil {
		return TemplateTypeSteps
	}
	if tmpl.Script != nil {
		return TemplateTypeScript
	}
	if tmpl.Resource != nil {
		return TemplateTypeResource
	}
	return "Unknown"
}

// GetArtifactByName returns an input artifact by its name
func (in *Inputs) GetArtifactByName(name string) *Artifact {
	for _, art := range in.Artifacts {
		if art.Name == name {
			return &art
		}
	}
	return nil
}

// GetParameterByName returns an input parameter by its name
func (in *Inputs) GetParameterByName(name string) *Parameter {
	for _, param := range in.Parameters {
		if param.Name == name {
			return &param
		}
	}
	return nil
}

// HasOutputs returns whether or not there are any outputs
func (out *Outputs) HasOutputs() bool {
	if out.Result != nil {
		return true
	}
	if len(out.Artifacts) > 0 {
		return true
	}
	if len(out.Parameters) > 0 {
		return true
	}
	return false
}

// GetArtifactByName retrieves an artifact by its name
func (args *Arguments) GetArtifactByName(name string) *Artifact {
	for _, art := range args.Artifacts {
		if art.Name == name {
			return &art
		}
	}
	return nil
}

// GetParameterByName retrieves a parameter by its name
func (args *Arguments) GetParameterByName(name string) *Parameter {
	for _, param := range args.Parameters {
		if param.Name == name {
			return &param
		}
	}
	return nil
}

// HasLocation whether or not an artifact has a location defined
func (a *Artifact) HasLocation() bool {
	return a.S3 != nil || a.Git != nil || a.HTTP != nil || a.Artifactory != nil || a.Raw != nil
}

// DeepCopyInto is an custom deepcopy function to deal with our use of the interface{} type
func (in *WorkflowStep) DeepCopyInto(out *WorkflowStep) {
	inBytes, err := json.Marshal(in)
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(inBytes, out)
	if err != nil {
		panic(err)
	}
}

// GetTemplate retrieves a defined template by its name
func (wf *Workflow) GetTemplate(name string) *Template {
	for _, t := range wf.Spec.Templates {
		if t.Name == name {
			return &t
		}
	}
	return nil
}

// NodeID creates a deterministic node ID based on a node name
func (wf *Workflow) NodeID(name string) string {
	if name == wf.ObjectMeta.Name {
		return wf.ObjectMeta.Name
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%s-%v", wf.ObjectMeta.Name, h.Sum32())
}
