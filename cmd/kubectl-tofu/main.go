/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command kubectl-tofu is a kubectl plugin to approve plans, request runs and read
// the logs of the tofu-tf-operator. Installed in the PATH, it runs as `kubectl tofu`.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/cli"
)

// options are the connection flags shared by the commands, as in kubectl.
type options struct {
	kubeconfig string
	context    string
	namespace  string
}

// connect returns the clients and the namespace to use.
func (o *options) connect() (client.Client, kubernetes.Interface, string, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if o.kubeconfig != "" {
		rules.ExplicitPath = o.kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: o.context}
	if o.namespace != "" {
		overrides.Context.Namespace = o.namespace
	}
	config := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides)
	restConfig, err := config.ClientConfig()
	if err != nil {
		return nil, nil, "", err
	}
	namespace, _, err := config.Namespace()
	if err != nil {
		return nil, nil, "", err
	}

	scheme := runtime.NewScheme()
	if err := infrav1alpha1.AddToScheme(scheme); err != nil {
		return nil, nil, "", err
	}
	c, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, "", err
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, "", err
	}
	return c, clientset, namespace, nil
}

func main() {
	opts := &options{}
	root := &cobra.Command{
		Use:           "kubectl-tofu",
		Short:         "Approve plans, request runs and read the logs of tofu-tf-operator Workspaces",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.kubeconfig, "kubeconfig", "", "path to the kubeconfig file")
	root.PersistentFlags().StringVar(&opts.context, "context", "", "name of the kubeconfig context to use")
	root.PersistentFlags().StringVarP(&opts.namespace, "namespace", "n", "", "namespace of the Workspace or Run")

	var yes bool
	approve := &cobra.Command{
		Use:   "approve RUN|WORKSPACE",
		Short: "Show the plan awaiting approval and apply it once confirmed",
		Long: "Show the plan of a Run awaiting approval (or of the current Run of a Workspace) and,\n" +
			"once confirmed, approve its digest: only this plan can then be applied.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, namespace, err := opts.connect()
			if err != nil {
				return err
			}
			return cli.Approve(cmd.Context(), c, cmd.OutOrStdout(), cmd.InOrStdin(), namespace, args[0], yes)
		},
	}
	approve.Flags().BoolVarP(&yes, "yes", "y", false, "approve without asking for confirmation")

	reconcile := &cobra.Command{
		Use:   "reconcile WORKSPACE",
		Short: "Request a new run of a Workspace",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, namespace, err := opts.connect()
			if err != nil {
				return err
			}
			return cli.Reconcile(cmd.Context(), c, cmd.OutOrStdout(), namespace, args[0], time.Now())
		},
	}

	var step string
	var follow bool
	logs := &cobra.Command{
		Use:   "logs RUN|WORKSPACE",
		Short: "Print the logs of a step of a Run (by default the last one)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, clientset, namespace, err := opts.connect()
			if err != nil {
				return err
			}
			run, err := cli.ResolveRun(cmd.Context(), c, namespace, args[0])
			if err != nil {
				return err
			}
			job, err := cli.StepJob(run, step)
			if err != nil {
				return err
			}
			return printJobLogs(cmd.Context(), clientset, cmd.OutOrStdout(), namespace, job, follow)
		},
	}
	logs.Flags().StringVar(&step, "step", "", "step to show: plan or apply")
	logs.Flags().BoolVarP(&follow, "follow", "f", false, "follow the logs")

	root.AddCommand(approve, reconcile, logs)
	if err := root.ExecuteContext(context.Background()); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

// printJobLogs copies the logs of the runner container of a Job.
func printJobLogs(ctx context.Context, clientset kubernetes.Interface, out io.Writer,
	namespace, job string, follow bool) error {
	selector := metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + job}
	pods, err := clientset.CoreV1().Pods(namespace).List(ctx, selector)
	if err != nil {
		return err
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("no pod found for the Job %s", job)
	}
	stream, err := clientset.CoreV1().Pods(namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		Container: "runner",
		Follow:    follow,
	}).Stream(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	_, err = io.Copy(out, stream)
	return err
}
