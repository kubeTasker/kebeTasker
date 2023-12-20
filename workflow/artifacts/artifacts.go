package executor

import (
	wfv1 "github.com/kubeTasker/kubeTasker/pkg/apis/workflow/v1alpha1"
)

// ArtifactDriver is the interface for loading and saving of artifacts
type ArtifactDriver interface {
	Load(inputArtifact *wfv1.Artifact, path string) error

	Save(path string, outputArtifact *wfv1.Artifact) error
}
