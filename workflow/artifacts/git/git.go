package git

import (
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"strings"

	"github.com/kubeTasker/kubeTasker/errors"
	wfv1 "github.com/kubeTasker/kubeTasker/pkg/apis/workflow/v1alpha1"
	"github.com/kubeTasker/kubeTasker/workflow/common"
)

// GitArtifactDriver is the artifact driver for a git repo
type GitArtifactDriver struct {
	Username string
	Password string
}

// Load download artifacts from an git URL
func (g *GitArtifactDriver) Load(inputArtifact *wfv1.Artifact, path string) error {
	if g.Username != "" || g.Password != "" {
		insecureURL, err := url.Parse(inputArtifact.Git.Repo)
		if err != nil {
			return errors.InternalWrapError(err)
		}
		insecureURL.User = url.UserPassword(g.Username, g.Password)
		tmpfile, err := ioutil.TempFile("", "git-cred-")
		if err != nil {
			return errors.InternalWrapError(err)
		}
		defer func() {
			_ = os.Remove(tmpfile.Name())
		}()
		content := []byte(insecureURL.String() + "\n")
		if _, err := tmpfile.Write(content); err != nil {
			return errors.InternalWrapError(err)
		}
		if err := tmpfile.Close(); err != nil {
			return errors.InternalWrapError(err)
		}
		err = common.RunCommand("git", "config", "--global", "credential.helper", fmt.Sprintf("store --file=%s", tmpfile.Name()))
		if err != nil {
			return err
		}
		defer func() {
			_ = common.RunCommand("git", "config", "--global", "--remove-section", "credential")
		}()
	}
	err := common.RunCommand("git", "clone", inputArtifact.Git.Repo, path)
	if err != nil {
		lines := strings.Split(err.Error(), "\n")
		if len(lines) > 1 {
			// give only the last, most-useful error line from git
			return errors.New(errors.CodeBadRequest, lines[len(lines)-1])
		}
		return err
	}
	if inputArtifact.Git.Revision != "" {
		err := common.RunCommand("git", "-C", path, "checkout", inputArtifact.Git.Revision)
		if err != nil {
			return err
		}
	}
	return nil
}

// Save is unsupported for git output artifacts
func (g *GitArtifactDriver) Save(path string, outputArtifact *wfv1.Artifact) error {
	return errors.Errorf(errors.CodeBadRequest, "Git output artifacts unsupported")
}
