package commands

import (
	"time"

	"github.com/kubeTasker/kubeTasker/util/stats"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

func init() {
	RootCmd.AddCommand(waitCmd)
}

var waitCmd = &cobra.Command{
	Use:   "wait",
	Short: "wait for main container to finish and save artifacts",
	Run: func(cmd *cobra.Command, args []string) {
		err := waitContainer()
		if err != nil {
			log.Fatal(err)
		}
	},
}

func waitContainer() error {
	wfExecutor := initExecutor()
	defer wfExecutor.HandleError()
	defer stats.LogStats()
	stats.StartStatsTicker(5 * time.Minute)

	err := wfExecutor.Wait()
	if err != nil {
		wfExecutor.AddError(err)
	}
	err = wfExecutor.SaveArtifacts()
	if err != nil {
		wfExecutor.AddError(err)
		return err
	}

	err = wfExecutor.SaveParameters()
	if err != nil {
		wfExecutor.AddError(err)
		return err
	}

	err = wfExecutor.CaptureScriptResult()
	if err != nil {
		wfExecutor.AddError(err)
		return err
	}
	err = wfExecutor.AnnotateOutputs()
	if err != nil {
		wfExecutor.AddError(err)
		return err
	}
	return nil
}
