package start

import (
	"context"
	"math/rand"
	"os"
	"time"

	"github.com/openshift/library-go/pkg/controller/controllercmd"
	"github.com/openshift/library-go/pkg/serviceability"
	"github.com/spf13/cobra"
	"k8s.io/apiserver/pkg/util/logs"
	"k8s.io/client-go/pkg/version"
	"k8s.io/klog"

	"github.com/openshift/support-operator/pkg/controller"
)

func NewOperator() *cobra.Command {
	operator := &controller.Support{
		StoragePath: "/var/lib/support-operator",
		Interval:    10 * time.Minute,
		Endpoint:    "https://cloud.redhat.com/api/ingress/v1/upload",
	}
	cfg := controllercmd.NewControllerCommandConfig("openshift-support-operator", version.Get(), operator.Run)
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the operator",
		Run: func(cmd *cobra.Command, args []string) {
			// boiler plate for the "normal" command
			rand.Seed(time.Now().UTC().UnixNano())
			logs.InitLogs()
			defer logs.FlushLogs()
			defer serviceability.BehaviorOnPanic(os.Getenv("OPENSHIFT_ON_PANIC"), version.Get())()
			defer serviceability.Profile(os.Getenv("OPENSHIFT_PROFILE")).Stop()
			serviceability.StartProfiler()

			unstructured, config, configBytes, err := cfg.Config()
			if err != nil {
				klog.Fatal(err)
			}

			startingFileContent, observedFiles, err := cfg.AddDefaultRotationToConfig(config, configBytes)
			if err != nil {
				klog.Fatal(err)
			}

			exitOnChangeReactorCh := make(chan struct{})
			ctx := context.Background()
			ctx2, cancel := context.WithCancel(ctx)
			go func() {
				select {
				case <-exitOnChangeReactorCh:
					cancel()
				case <-ctx.Done():
					cancel()
				}
			}()

			builder := controllercmd.NewController("openshift-support-operator", operator.Run).
				WithKubeConfigFile(cmd.Flags().Lookup("kubeconfig").Value.String(), nil).
				WithLeaderElection(config.LeaderElection, "", "openshift-support-operator-lock").
				WithServer(config.ServingInfo, config.Authentication, config.Authorization).
				WithRestartOnChange(exitOnChangeReactorCh, startingFileContent, observedFiles...)

			if err := builder.Run(unstructured, ctx2); err != nil {
				klog.Fatal(err)
			}
		},
	}
	cmd.Flags().AddFlagSet(cfg.NewCommand().Flags())

	return cmd
}
