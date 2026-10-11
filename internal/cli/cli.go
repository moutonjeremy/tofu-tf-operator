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

// Package cli implements the commands of the kubectl-tofu plugin.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// ErrCancelled is returned when the user does not confirm an approval.
var ErrCancelled = errors.New("approval cancelled")

// ResolveRun returns the Run with the given name or, if there is none, the current
// (or else the last) Run of the Workspace with the given name.
func ResolveRun(ctx context.Context, c client.Client, namespace, name string) (*infrav1alpha1.Run, error) {
	run := &infrav1alpha1.Run{}
	err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, run)
	if err == nil {
		return run, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	ws := &infrav1alpha1.Workspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, ws); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("no Run nor Workspace named %s in namespace %s", name, namespace)
		}
		return nil, err
	}
	runName := ws.Status.CurrentRun
	if runName == "" && ws.Status.LastRun != nil {
		runName = ws.Status.LastRun.Name
	}
	if runName == "" {
		return nil, fmt.Errorf("the Workspace %s has no run yet", name)
	}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: runName}, run); err != nil {
		return nil, err
	}
	return run, nil
}

// Approve shows the plan of a Run awaiting approval and, once confirmed, approves its digest.
func Approve(ctx context.Context, c client.Client, out io.Writer, in io.Reader, namespace, name string, yes bool) error {
	run, err := ResolveRun(ctx, c, namespace, name)
	if err != nil {
		return err
	}
	if run.Status.Phase != infrav1alpha1.PhaseAwaitingApproval || run.Status.Plan == nil {
		return fmt.Errorf("the run %s is %s, not awaiting approval", run.Name, phaseOf(run))
	}

	plan := run.Status.Plan
	_, _ = fmt.Fprintf(out, "Run %s of Workspace %s (%s, %s)\n", run.Name, run.Spec.WorkspaceRef.Name, run.Spec.Trigger, run.Spec.Operation)
	_, _ = fmt.Fprintf(out, "Revision: %s\n", run.Spec.Revision)
	_, _ = fmt.Fprintf(out, "Plan: %d to add, %d to change, %d to destroy", plan.Add, plan.Change, plan.Destroy)
	if plan.Import > 0 {
		_, _ = fmt.Fprintf(out, ", %d to import", plan.Import)
	}
	_, _ = fmt.Fprintf(out, "\nFull plan: kubectl -n %s logs job/%s\n", run.Namespace, run.Status.PlanJob)

	if !yes {
		_, _ = fmt.Fprint(out, "Apply this plan? [y/N] ")
		answer, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
			return ErrCancelled
		}
	}

	patch := client.MergeFrom(run.DeepCopy())
	run.Spec.ApprovedPlan = plan.Digest
	if err := c.Patch(ctx, run, patch); err != nil {
		return fmt.Errorf("approving the run %s: %w", run.Name, err)
	}
	_, _ = fmt.Fprintf(out, "run %s approved (%s)\n", run.Name, plan.Digest)
	return nil
}

// Reconcile asks for a new run of a Workspace by setting its requested-at annotation.
func Reconcile(ctx context.Context, c client.Client, out io.Writer, namespace, name string, now time.Time) error {
	ws := &infrav1alpha1.Workspace{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, ws); err != nil {
		return err
	}
	patch := client.MergeFrom(ws.DeepCopy())
	if ws.Annotations == nil {
		ws.Annotations = map[string]string{}
	}
	value := now.UTC().Format(time.RFC3339Nano)
	ws.Annotations[infrav1alpha1.RequestedAtAnnotation] = value
	if err := c.Patch(ctx, ws, patch); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "run requested for Workspace %s (%s=%s)\n", name, infrav1alpha1.RequestedAtAnnotation, value)
	return nil
}

// StepJob returns the Job of a step of a Run. Without a step, it is the last started one.
func StepJob(run *infrav1alpha1.Run, step string) (string, error) {
	switch step {
	case "":
		if run.Status.ApplyJob != "" {
			return run.Status.ApplyJob, nil
		}
		if run.Status.PlanJob != "" {
			return run.Status.PlanJob, nil
		}
		return "", fmt.Errorf("the run %s has not started yet", run.Name)
	case "plan":
		if run.Status.PlanJob != "" {
			return run.Status.PlanJob, nil
		}
	case "apply":
		if run.Status.ApplyJob != "" {
			return run.Status.ApplyJob, nil
		}
	default:
		return "", fmt.Errorf("unknown step %q, expected plan or apply", step)
	}
	return "", fmt.Errorf("the run %s has no %s step yet", run.Name, step)
}

func phaseOf(run *infrav1alpha1.Run) string {
	if run.Status.Phase == "" {
		return string(infrav1alpha1.PhasePending)
	}
	return string(run.Status.Phase)
}
