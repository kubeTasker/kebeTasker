package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"

	"github.com/kubeTasker/kubeTasker/errors"
	workflow "github.com/kubeTasker/kubeTasker/pkg/apis/workflow"
	wfv1 "github.com/kubeTasker/kubeTasker/pkg/apis/workflow/v1alpha1"
	"github.com/kubeTasker/kubeTasker/workflow/common"
	log "github.com/sirupsen/logrus"
	"github.com/valyala/fasttemplate"
	apiv1 "k8s.io/api/core/v1"
	apierr "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reusable k8s pod spec portions used in workflow pods
var (
	volumePodMetadata = apiv1.Volume{
		Name: common.PodMetadataVolumeName,
		VolumeSource: apiv1.VolumeSource{
			DownwardAPI: &apiv1.DownwardAPIVolumeSource{
				Items: []apiv1.DownwardAPIVolumeFile{
					{
						Path: common.PodMetadataAnnotationsVolumePath,
						FieldRef: &apiv1.ObjectFieldSelector{
							APIVersion: "v1",
							FieldPath:  "metadata.annotations",
						},
					},
				},
			},
		},
	}
	volumeMountPodMetadata = apiv1.VolumeMount{
		Name:      volumePodMetadata.Name,
		MountPath: common.PodMetadataMountPath,
	}

	hostPathDir    = apiv1.HostPathDirectory
	hostPathSocket = apiv1.HostPathSocket

	volumeDockerLib = apiv1.Volume{
		Name: common.DockerLibVolumeName,
		VolumeSource: apiv1.VolumeSource{
			HostPath: &apiv1.HostPathVolumeSource{
				Path: common.DockerLibHostPath,
				Type: &hostPathDir,
			},
		},
	}
	volumeMountDockerLib = apiv1.VolumeMount{
		Name:      volumeDockerLib.Name,
		MountPath: volumeDockerLib.VolumeSource.HostPath.Path,
		ReadOnly:  true,
	}

	volumeDockerSock = apiv1.Volume{
		Name: common.DockerSockVolumeName,
		VolumeSource: apiv1.VolumeSource{
			HostPath: &apiv1.HostPathVolumeSource{
				Path: "/var/run/docker.sock",
				Type: &hostPathSocket,
			},
		},
	}
	volumeMountDockerSock = apiv1.VolumeMount{
		Name:      volumeDockerSock.Name,
		MountPath: "/var/run/docker.sock",
		ReadOnly:  true,
	}

	// execEnvVars exposes various pod information as environment variables to the exec container
	execEnvVars = []apiv1.EnvVar{
		envFromField(common.EnvVarPodIP, "status.podIP"),
		envFromField(common.EnvVarPodName, "metadata.name"),
		envFromField(common.EnvVarNamespace, "metadata.namespace"),
	}
)

// envFromField is a helper to return a EnvVar with the name and field
func envFromField(envVarName, fieldPath string) apiv1.EnvVar {
	return apiv1.EnvVar{
		Name: envVarName,
		ValueFrom: &apiv1.EnvVarSource{
			FieldRef: &apiv1.ObjectFieldSelector{
				APIVersion: "v1",
				FieldPath:  fieldPath,
			},
		},
	}
}

func (woc *wfOperationCtx) createWorkflowPod(nodeName string, mainCtr apiv1.Container, tmpl *wfv1.Template) (*apiv1.Pod, error) {
	nodeID := woc.wf.NodeID(nodeName)
	woc.log.Debugf("Creating Pod: %s (%s)", nodeName, nodeID)
	tmpl = tmpl.DeepCopy()
	mainCtr.Name = common.MainContainerName
	pod := apiv1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeID,
			Labels: map[string]string{
				common.LabelKeyWorkflow:  woc.wf.ObjectMeta.Name, // Allows filtering by pods related to specific workflow
				common.LabelKeyCompleted: "false",                // Allows filtering by incomplete workflow pods
			},
			Annotations: map[string]string{
				common.AnnotationKeyNodeName: nodeName,
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(woc.wf, wfv1.SchemeGroupVersion.WithKind(workflow.Kind)),
			},
		},
		Spec: apiv1.PodSpec{
			RestartPolicy: apiv1.RestartPolicyNever,
			Containers: []apiv1.Container{
				mainCtr,
			},
			Volumes: []apiv1.Volume{
				volumePodMetadata,
				volumeDockerLib,
				volumeDockerSock,
			},
			ActiveDeadlineSeconds: tmpl.ActiveDeadlineSeconds,
			ServiceAccountName:    woc.wf.Spec.ServiceAccountName,
			ImagePullSecrets:      woc.wf.Spec.ImagePullSecrets,
		},
	}
	if woc.controller.Config.InstanceID != "" {
		pod.ObjectMeta.Labels[common.LabelKeyControllerInstanceID] = woc.controller.Config.InstanceID
	}

	if tmpl.GetType() != wfv1.TemplateTypeResource {
		waitCtr, err := woc.newWaitContainer(tmpl)
		if err != nil {
			return nil, err
		}
		pod.Spec.Containers = append(pod.Spec.Containers, *waitCtr)
	}

	if len(tmpl.Inputs.Artifacts) > 0 || tmpl.GetType() == wfv1.TemplateTypeScript {
		initCtr := woc.newInitContainer(tmpl)
		pod.Spec.InitContainers = []apiv1.Container{initCtr}
	}

	woc.addSchedulingConstraints(&pod, tmpl)

	err := woc.addVolumeReferences(&pod, tmpl)
	if err != nil {
		return nil, err
	}

	err = woc.addInputArtifactsVolumes(&pod, tmpl)
	if err != nil {
		return nil, err
	}

	err = woc.addArchiveLocation(&pod, tmpl)
	if err != nil {
		return nil, err
	}

	if tmpl.GetType() == wfv1.TemplateTypeScript {
		addExecutorStagingVolume(&pod)
	}

	// addSidecars should be called after all volumes have been manipulated
	// in the main container (in case sidecar requires volume mount mirroring)
	err = addSidecars(&pod, tmpl)
	if err != nil {
		return nil, err
	}

	tmplBytes, err := json.Marshal(tmpl)
	if err != nil {
		return nil, err
	}
	err = verifyResolvedVariables(string(tmplBytes))
	if err != nil {
		return nil, err
	}
	pod.ObjectMeta.Annotations[common.AnnotationKeyTemplate] = string(tmplBytes)

	created, err := woc.controller.kubeclientset.CoreV1().Pods(woc.wf.ObjectMeta.Namespace).Create(context.TODO(), &pod, metav1.CreateOptions{})
	if err != nil {
		if apierr.IsAlreadyExists(err) {
			woc.log.Infof("Skipped pod %s (%s) creation: already exists", nodeName, nodeID)
			return created, nil
		}
		woc.log.Infof("Failed to create pod %s (%s): %v", nodeName, nodeID, err)
		return nil, errors.InternalWrapError(err)
	}
	woc.log.Infof("Created pod: %s (%s)", nodeName, created.Name)
	return created, nil
}

func (woc *wfOperationCtx) newInitContainer(tmpl *wfv1.Template) apiv1.Container {
	ctr := woc.newExecContainer(common.InitContainerName, false)
	ctr.Command = []string{"taskerexec"}
	ctr.Args = []string{"init"}
	ctr.VolumeMounts = []apiv1.VolumeMount{
		volumeMountPodMetadata,
	}
	return *ctr
}

func (woc *wfOperationCtx) newWaitContainer(tmpl *wfv1.Template) (*apiv1.Container, error) {
	ctr := woc.newExecContainer(common.WaitContainerName, false)
	ctr.Command = []string{"taskerexec"}
	ctr.Args = []string{"wait"}
	ctr.VolumeMounts = []apiv1.VolumeMount{
		volumeMountPodMetadata,
		volumeMountDockerLib,
		volumeMountDockerSock,
	}
	return ctr, nil
}

func (woc *wfOperationCtx) newExecContainer(name string, privileged bool) *apiv1.Container {
	exec := apiv1.Container{
		Name:  name,
		Image: woc.controller.Config.ExecutorImage,
		Env:   execEnvVars,
		SecurityContext: &apiv1.SecurityContext{
			Privileged: &privileged,
		},
	}
	if woc.controller.Config.ExecutorResources != nil {
		exec.Resources = *woc.controller.Config.ExecutorResources
	}
	return &exec
}

// addSchedulingConstraints applies any node selectors or affinity rules to the pod, either set in the workflow or the template
func (woc *wfOperationCtx) addSchedulingConstraints(pod *apiv1.Pod, tmpl *wfv1.Template) {
	if len(tmpl.NodeSelector) > 0 {
		pod.Spec.NodeSelector = tmpl.NodeSelector
	} else if len(woc.wf.Spec.NodeSelector) > 0 {
		pod.Spec.NodeSelector = woc.wf.Spec.NodeSelector
	}
	if tmpl.Affinity != nil {
		pod.Spec.Affinity = tmpl.Affinity
	} else if woc.wf.Spec.Affinity != nil {
		pod.Spec.Affinity = woc.wf.Spec.Affinity
	}
}

// addVolumeReferences adds any volumeMounts that a container/sidecar is referencing, to the pod.spec.volumes
func (woc *wfOperationCtx) addVolumeReferences(pod *apiv1.Pod, tmpl *wfv1.Template) error {
	if tmpl.Container == nil && len(tmpl.Sidecars) == 0 {
		return nil
	}
	addVolumeRef := func(volMounts []apiv1.VolumeMount) error {
		for _, volMnt := range volMounts {
			vol := getVolByName(volMnt.Name, woc.wf)
			if vol == nil {
				return errors.Errorf(errors.CodeBadRequest, "volume '%s' not found in workflow spec", volMnt.Name)
			}
			found := false
			for _, v := range pod.Spec.Volumes {
				if v.Name == vol.Name {
					found = true
					break
				}
			}
			if !found {
				if pod.Spec.Volumes == nil {
					pod.Spec.Volumes = make([]apiv1.Volume, 0)
				}
				pod.Spec.Volumes = append(pod.Spec.Volumes, *vol)
			}
		}
		return nil
	}
	if tmpl.Container != nil {
		err := addVolumeRef(tmpl.Container.VolumeMounts)
		if err != nil {
			return err
		}
	}
	for _, sidecar := range tmpl.Sidecars {
		err := addVolumeRef(sidecar.VolumeMounts)
		if err != nil {
			return err
		}
	}
	return nil
}

// getVolByName is a helper to retrieve a volume by its name, either from the volumes or claims section
func getVolByName(name string, wf *wfv1.Workflow) *apiv1.Volume {
	for _, vol := range wf.Spec.Volumes {
		if vol.Name == name {
			return &vol
		}
	}
	for _, pvc := range wf.Status.PersistentVolumeClaims {
		if pvc.Name == name {
			return &pvc
		}
	}
	return nil
}

// addInputArtifactVolumes sets up the artifacts volume to the pod to support input artifacts to containers.
func (woc *wfOperationCtx) addInputArtifactsVolumes(pod *apiv1.Pod, tmpl *wfv1.Template) error {
	if len(tmpl.Inputs.Artifacts) == 0 {
		return nil
	}
	artVol := apiv1.Volume{
		Name: "input-artifacts",
		VolumeSource: apiv1.VolumeSource{
			EmptyDir: &apiv1.EmptyDirVolumeSource{},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, artVol)

	for i, initCtr := range pod.Spec.InitContainers {
		if initCtr.Name == common.InitContainerName {
			volMount := apiv1.VolumeMount{
				Name:      artVol.Name,
				MountPath: common.ExecutorArtifactBaseDir,
			}
			initCtr.VolumeMounts = append(initCtr.VolumeMounts, volMount)

			if tmpl.Container != nil {
				for _, mnt := range tmpl.Container.VolumeMounts {
					mnt.MountPath = path.Join(common.InitContainerMainFilesystemDir, mnt.MountPath)
					initCtr.VolumeMounts = append(initCtr.VolumeMounts, mnt)
				}
			}
			pod.Spec.InitContainers[i] = initCtr
			break
		}
	}

	mainCtrIndex := 0
	var mainCtr *apiv1.Container
	for i, ctr := range pod.Spec.Containers {
		if ctr.Name == common.MainContainerName {
			mainCtrIndex = i
			mainCtr = &pod.Spec.Containers[i]
		}
	}
	if mainCtr == nil {
		panic("Could not find main container in pod spec")
	}
	for _, art := range tmpl.Inputs.Artifacts {
		if art.Path == "" {
			return errors.Errorf(errors.CodeBadRequest, "inputs.artifacts.%s did not specify a path", art.Name)
		}
		overlap := common.FindOverlappingVolume(tmpl, art.Path)
		if overlap != nil {
			woc.log.Debugf("skip volume mount of %s (%s): overlaps with mount %s at %s",
				art.Name, art.Path, overlap.Name, overlap.MountPath)
			continue
		}
		volMount := apiv1.VolumeMount{
			Name:      artVol.Name,
			MountPath: art.Path,
			SubPath:   art.Name,
		}
		if mainCtr.VolumeMounts == nil {
			mainCtr.VolumeMounts = make([]apiv1.VolumeMount, 0)
		}
		mainCtr.VolumeMounts = append(mainCtr.VolumeMounts, volMount)
	}
	pod.Spec.Containers[mainCtrIndex] = *mainCtr
	return nil
}

// addArchiveLocation updates the template with artifact repository information configured in the controller.
func (woc *wfOperationCtx) addArchiveLocation(pod *apiv1.Pod, tmpl *wfv1.Template) error {
	if tmpl.ArchiveLocation != nil {
		return nil
	}
	tmpl.ArchiveLocation = &wfv1.ArtifactLocation{}
	if woc.controller.Config.ArtifactRepository.S3 != nil {
		log.Debugf("Setting s3 artifact repository information")
		keyPrefix := ""
		if woc.controller.Config.ArtifactRepository.S3.KeyPrefix != "" {
			keyPrefix = woc.controller.Config.ArtifactRepository.S3.KeyPrefix + "/"
		}
		artLocationKey := fmt.Sprintf("%s%s/%s", keyPrefix, woc.wf.ObjectMeta.Name, pod.ObjectMeta.Name)
		tmpl.ArchiveLocation.S3 = &wfv1.S3Artifact{
			S3Bucket: woc.controller.Config.ArtifactRepository.S3.S3Bucket,
			Key:      artLocationKey,
		}
	} else if woc.controller.Config.ArtifactRepository.Artifactory != nil {
		log.Debugf("Setting artifactory artifact repository information")
		repoURL := ""
		if woc.controller.Config.ArtifactRepository.Artifactory.RepoURL != "" {
			repoURL = woc.controller.Config.ArtifactRepository.Artifactory.RepoURL + "/"
		}
		artURL := fmt.Sprintf("%s%s/%s", repoURL, woc.wf.ObjectMeta.Name, pod.ObjectMeta.Name)
		tmpl.ArchiveLocation.Artifactory = &wfv1.ArtifactoryArtifact{
			ArtifactoryAuth: woc.controller.Config.ArtifactRepository.Artifactory.ArtifactoryAuth,
			URL:             artURL,
		}
	} else {
		for _, art := range tmpl.Outputs.Artifacts {
			if !art.HasLocation() {
				log.Errorf("artifact has no location details:%#v", art)
				return errors.Errorf(errors.CodeBadRequest, "controller is not configured with a default archive location")
			}
		}
	}
	return nil
}

// addExecutorStagingVolume sets up a shared staging volume between the init container
// and main container
func addExecutorStagingVolume(pod *apiv1.Pod) {
	volName := "kubetasker-staging"
	stagingVol := apiv1.Volume{
		Name: volName,
		VolumeSource: apiv1.VolumeSource{
			EmptyDir: &apiv1.EmptyDirVolumeSource{},
		},
	}
	pod.Spec.Volumes = append(pod.Spec.Volumes, stagingVol)

	for i, initCtr := range pod.Spec.InitContainers {
		if initCtr.Name == common.InitContainerName {
			volMount := apiv1.VolumeMount{
				Name:      volName,
				MountPath: common.ExecutorStagingEmptyDir,
			}
			initCtr.VolumeMounts = append(initCtr.VolumeMounts, volMount)
			pod.Spec.InitContainers[i] = initCtr
			break
		}
	}
	found := false
	for i, ctr := range pod.Spec.Containers {
		if ctr.Name == common.MainContainerName {
			volMount := apiv1.VolumeMount{
				Name:      volName,
				MountPath: common.ExecutorStagingEmptyDir,
			}
			if ctr.VolumeMounts == nil {
				ctr.VolumeMounts = []apiv1.VolumeMount{volMount}
			} else {
				ctr.VolumeMounts = append(ctr.VolumeMounts, volMount)
			}
			pod.Spec.Containers[i] = ctr
			found = true
			break
		}
	}
	if !found {
		panic("Unable to locate main container")
	}
}

// addSidecars adds all sidecars to the pod spec of the step.
func addSidecars(pod *apiv1.Pod, tmpl *wfv1.Template) error {
	if len(tmpl.Sidecars) == 0 {
		return nil
	}
	var mainCtr *apiv1.Container
	for _, ctr := range pod.Spec.Containers {
		if ctr.Name != common.MainContainerName {
			continue
		}
		mainCtr = &ctr
		break
	}
	if mainCtr == nil {
		panic("Unable to locate main container")
	}
	for _, sidecar := range tmpl.Sidecars {
		if sidecar.MirrorVolumeMounts != nil && *sidecar.MirrorVolumeMounts {
			for _, volMnt := range mainCtr.VolumeMounts {
				if sidecar.VolumeMounts == nil {
					sidecar.VolumeMounts = make([]apiv1.VolumeMount, 0)
				}
				sidecar.VolumeMounts = append(sidecar.VolumeMounts, volMnt)
			}
		}
		pod.Spec.Containers = append(pod.Spec.Containers, sidecar.Container)
	}
	return nil
}

// verifyResolvedVariables is a helper to ensure all {{variables}} have been resolved
func verifyResolvedVariables(tmplStr string) error {
	var unresolvedErr error
	fstTmpl := fasttemplate.New(tmplStr, "{{", "}}")
	fstTmpl.ExecuteFuncString(func(w io.Writer, tag string) (int, error) {
		unresolvedErr = errors.Errorf(errors.CodeBadRequest, "failed to resolve {{%s}}", tag)
		return 0, nil
	})
	return unresolvedErr
}
