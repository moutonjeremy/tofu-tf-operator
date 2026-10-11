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

// Command runner executes one step (plan or apply) of a Run. It is started by
// the Run controller in a Job and reports its result in the termination message.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/labbs/tofu-tf-operator/internal/engine"
	"github.com/labbs/tofu-tf-operator/internal/runner"
)

// terminationLog is where the kubelet reads the termination message.
const terminationLog = "/dev/termination-log"

func main() {
	if len(os.Args) != 2 || (os.Args[1] != string(runner.StepPlan) && os.Args[1] != string(runner.StepApply)) {
		_, _ = fmt.Fprintf(os.Stderr, "usage: %s plan|apply\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	step := runner.Step(os.Args[1])

	// SIGTERM is sent when the Job times out or is deleted: interrupt the engine
	// so that it persists the state and releases the lock.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	result, err := run(ctx, step)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stdout, "Error: %v\n", err)
		result = runner.Result{Step: step, Error: err.Error()}
	}
	if err := os.WriteFile(terminationLog, result.Encode(), 0o644); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "writing the termination message: %v\n", err)
	}
	if result.Error != "" {
		os.Exit(1)
	}
}

func run(ctx context.Context, step runner.Step) (runner.Result, error) {
	cfg, err := runner.ConfigFromEnv()
	if err != nil {
		return runner.Result{}, err
	}

	installer := engine.NewInstaller(filepath.Join(cfg.WorkDir, "engines"))
	bin, err := installer.Ensure(ctx, cfg.Engine, cfg.EngineVersion)
	if err != nil {
		return runner.Result{}, err
	}

	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return runner.Result{}, fmt.Errorf("loading the in-cluster configuration: %w", err)
	}
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return runner.Result{}, err
	}

	r := &runner.Runner{
		Config: cfg,
		Store:  &runner.SecretStore{Secrets: client.CoreV1(), Config: cfg},
		Bin:    bin,
		Log:    os.Stdout,
	}
	return r.Run(ctx, step), nil
}
