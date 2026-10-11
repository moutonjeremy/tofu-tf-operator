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

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/metrics"
	"github.com/labbs/tofu-tf-operator/internal/runner"
)

const (
	// jobNameLabel is set by the Job controller on the pods of a Job.
	jobNameLabel      = "batch.kubernetes.io/job-name"
	jobTimeoutMessage = "the step exceeded its timeout"
)

// Notifier sends the events of a Run to the providers of its Workspace.
type Notifier interface {
	Notify(ctx context.Context, ws *infrav1alpha1.Workspace, run *infrav1alpha1.Run, event infrav1alpha1.NotificationEvent)
}

// RunReconciler drives a Run through its plan and apply Jobs.
// It is the only writer of the Run status.
type RunReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// RunnerImage is the default image of the runner Jobs.
	RunnerImage string
	// Notifier is optional.
	Notifier Notifier
}

// +kubebuilder:rbac:groups=infra.labbs.fr,resources=runs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=runs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=runs/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=notificationproviders,verbs=get;list;watch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile moves a Run to its next phase.
func (r *RunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	run := &infrav1alpha1.Run{}
	if err := r.Get(ctx, req.NamespacedName, run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if run.Status.Phase.IsFinished() {
		return ctrl.Result{}, nil
	}

	before := run.DeepCopy()
	result, err := r.reconcileRun(ctx, run)
	// The status update sets a new resourceVersion only when it is saved.
	if run.ResourceVersion != before.ResourceVersion && run.Status.Phase != before.Status.Phase {
		r.observeTransition(ctx, before, run)
	}
	return result, err
}

func (r *RunReconciler) reconcileRun(ctx context.Context, run *infrav1alpha1.Run) (ctrl.Result, error) {
	ws := &infrav1alpha1.Workspace{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: run.Spec.WorkspaceRef.Name}, ws); err != nil {
		if apierrors.IsNotFound(err) {
			return r.finish(ctx, run, nil, infrav1alpha1.PhaseFailed, "the Workspace does not exist")
		}
		return ctrl.Result{}, err
	}

	switch run.Status.Phase {
	case "", infrav1alpha1.PhasePending:
		if reason, ok := supersedeRequested(run); ok {
			return r.finish(ctx, run, ws, infrav1alpha1.PhaseSuperseded, reason)
		}
		return r.startStep(ctx, run, ws, runner.StepPlan)

	case infrav1alpha1.PhasePlanning:
		return r.reconcileStep(ctx, run, ws, runner.StepPlan)

	case infrav1alpha1.PhaseAwaitingApproval:
		if reason, ok := supersedeRequested(run); ok {
			return r.finish(ctx, run, ws, infrav1alpha1.PhaseSuperseded, reason)
		}
		if run.Spec.ApprovedPlan == "" {
			return ctrl.Result{}, nil
		}
		if !run.PlanApproved() {
			message := fmt.Sprintf("approvedPlan %s is not the digest of the plan; approve with: %s",
				run.Spec.ApprovedPlan, run.ApproveCommand())
			if run.Status.Message == message {
				return ctrl.Result{}, nil
			}
			run.Status.Message = message
			return r.updateStatus(ctx, run, ws, nil)
		}
		return r.startStep(ctx, run, ws, runner.StepApply)

	case infrav1alpha1.PhaseApplying:
		return r.reconcileStep(ctx, run, ws, runner.StepApply)
	}
	return ctrl.Result{}, fmt.Errorf("unknown phase %q", run.Status.Phase)
}

// startStep creates the Job of a step and moves the Run to the matching phase.
func (r *RunReconciler) startStep(ctx context.Context, run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace,
	step runner.Step, notifications ...infrav1alpha1.NotificationEvent) (ctrl.Result, error) {
	job, err := r.ensureJob(ctx, run, ws, step)
	if err != nil {
		return ctrl.Result{}, err
	}
	now := metav1.Now()
	if step == runner.StepPlan {
		run.Status.Phase = infrav1alpha1.PhasePlanning
		run.Status.PlanJob = job.Name
		run.Status.StartedAt = &now
	} else {
		run.Status.Phase = infrav1alpha1.PhaseApplying
		run.Status.ApplyJob = job.Name
	}
	run.Status.Message = ""
	return r.updateStatus(ctx, run, ws, notifications)
}

// ensureJob creates the Job of a step unless it exists. Job names are deterministic,
// so a step is never started twice.
func (r *RunReconciler) ensureJob(ctx context.Context, run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace, step runner.Step) (*batchv1.Job, error) {
	job, err := buildStepJob(run, ws, step, r.RunnerImage)
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetControllerReference(run, job, r.Scheme); err != nil {
		return nil, err
	}
	err = r.Create(ctx, job)
	switch {
	case apierrors.IsAlreadyExists(err):
		return job, nil
	case err != nil:
		return nil, fmt.Errorf("creating the %s Job: %w", step, err)
	}
	logf.FromContext(ctx).Info("step started", "step", step, "job", job.Name)
	r.event(run, ws, corev1.EventTypeNormal, "StepStarted", string(step), "Started the %s Job %s", step, job.Name)
	return job, nil
}

// reconcileStep handles the end of the Job of a step.
func (r *RunReconciler) reconcileStep(ctx context.Context, run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace, step runner.Step) (ctrl.Result, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: stepJobName(run.Name, step)}, job)
	if apierrors.IsNotFound(err) {
		// The status was written but the Job is gone (or not yet in the cache): create it again.
		_, err = r.ensureJob(ctx, run, ws, step)
		return ctrl.Result{}, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	finished, failure := jobOutcome(job)
	if !finished {
		return ctrl.Result{}, nil
	}

	result, err := r.stepResult(ctx, job, step, failure)
	if err != nil {
		return ctrl.Result{}, err
	}
	switch {
	case result.Error == "":
	case result.StalePlan:
		run.Status.FailureReason = infrav1alpha1.FailureStalePlan
	case failure == jobTimeoutMessage:
		run.Status.FailureReason = infrav1alpha1.FailureTimeout
	default:
		run.Status.FailureReason = infrav1alpha1.FailureStepFailed
	}
	destroy := run.Spec.Operation == infrav1alpha1.OperationDestroy

	if step == runner.StepApply {
		if result.Error != "" {
			event := infrav1alpha1.EventApplyFailed
			if destroy {
				event = infrav1alpha1.EventDestroyFailed
			}
			return r.finish(ctx, run, ws, infrav1alpha1.PhaseFailed, failureMessage(result), event)
		}
		event := infrav1alpha1.EventApplySucceeded
		if destroy {
			event = infrav1alpha1.EventDestroySucceeded
		}
		return r.finish(ctx, run, ws, infrav1alpha1.PhaseApplied, "", event)
	}

	if result.Error != "" {
		return r.finish(ctx, run, ws, infrav1alpha1.PhaseFailed, failureMessage(result), infrav1alpha1.EventPlanFailed)
	}
	if result.Plan == nil || !result.Plan.HasChanges {
		run.Status.Plan = result.Plan
		return r.finish(ctx, run, ws, infrav1alpha1.PhaseNoChanges, "")
	}
	run.Status.Plan = result.Plan

	notifications := []infrav1alpha1.NotificationEvent{infrav1alpha1.EventPlanHasChanges}
	if run.Spec.Trigger == infrav1alpha1.TriggerInterval {
		notifications = append(notifications, infrav1alpha1.EventDriftDetected)
	}
	if reason, ok := supersedeRequested(run); ok {
		return r.finish(ctx, run, ws, infrav1alpha1.PhaseSuperseded, reason)
	}
	if run.Spec.AutoApprove || run.PlanApproved() {
		return r.startStep(ctx, run, ws, runner.StepApply, notifications...)
	}
	run.Status.Phase = infrav1alpha1.PhaseAwaitingApproval
	run.Status.Message = "approve with: " + run.ApproveCommand()
	notifications = append(notifications, infrav1alpha1.EventApprovalRequired)
	return r.updateStatus(ctx, run, ws, notifications)
}

// stepResult reads the result written by the runner in the termination message
// of the pod. failure is the reason of a failed Job, if any.
func (r *RunReconciler) stepResult(ctx context.Context, job *batchv1.Job, step runner.Step, failure string) (runner.Result, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(job.Namespace), client.MatchingLabels{jobNameLabel: job.Name}); err != nil {
		return runner.Result{}, err
	}
	var message string
	for _, pod := range pods.Items {
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name == runnerContainer && status.State.Terminated != nil {
				message = status.State.Terminated.Message
			}
		}
	}

	result, ok := runner.DecodeResult(message)
	switch {
	case ok && (failure == "" || result.Error != ""):
		return result, nil
	case failure != "":
		// The runner crashed or was killed: the message holds the end of its logs, if anything.
		return runner.Result{Step: step, Error: failure, LogTail: message}, nil
	default:
		return runner.Result{Step: step, Error: "the runner did not report a result", LogTail: message}, nil
	}
}

// jobOutcome tells whether a Job is finished and, if it failed, why.
func jobOutcome(job *batchv1.Job) (bool, string) {
	for _, c := range job.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case batchv1.JobComplete:
			return true, ""
		case batchv1.JobFailed:
			if c.Reason == batchv1.JobReasonDeadlineExceeded {
				return true, jobTimeoutMessage
			}
			return true, strings.TrimSpace("the runner failed: " + c.Message)
		}
	}
	return false, ""
}

// failureMessage summarizes a failed step for the Run status.
func failureMessage(result runner.Result) string {
	if result.LogTail == "" {
		return result.Error
	}
	return result.Error + "\n" + lastLines(result.LogTail, 20)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// finish moves the Run to a terminal phase.
func (r *RunReconciler) finish(ctx context.Context, run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace,
	phase infrav1alpha1.RunPhase, message string, notifications ...infrav1alpha1.NotificationEvent) (ctrl.Result, error) {
	now := metav1.Now()
	run.Status.Phase = phase
	run.Status.Message = message
	run.Status.CompletedAt = &now
	if run.Status.StartedAt == nil {
		run.Status.StartedAt = &now
	}
	eventType := corev1.EventTypeNormal
	if phase == infrav1alpha1.PhaseFailed {
		eventType = corev1.EventTypeWarning
	}
	note := "Run " + strings.ToLower(string(phase))
	if message != "" {
		note += ": " + firstLine(message)
	}
	r.event(run, ws, eventType, string(phase), "Finish", "%s", note)
	return r.updateStatus(ctx, run, ws, notifications)
}

// updateStatus writes the status, then sends the notifications that were not sent yet.
// Notifications are sent at most once: after the status recording them is saved.
func (r *RunReconciler) updateStatus(ctx context.Context, run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace,
	notifications []infrav1alpha1.NotificationEvent) (ctrl.Result, error) {
	var toSend []infrav1alpha1.NotificationEvent
	for _, event := range notifications {
		if !slices.Contains(run.Status.NotifiedEvents, event) {
			run.Status.NotifiedEvents = append(run.Status.NotifiedEvents, event)
			toSend = append(toSend, event)
		}
	}
	run.Status.ObservedGeneration = run.Generation
	if err := r.Status().Update(ctx, run); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	for _, event := range toSend {
		if event == infrav1alpha1.EventDriftDetected {
			metrics.DriftDetectedTotal.WithLabelValues(run.Namespace, run.Spec.WorkspaceRef.Name).Inc()
		}
		r.event(run, ws, corev1.EventTypeNormal, string(event), "Notify", "%s", planDescription(run.Status.Plan))
		if r.Notifier != nil && ws != nil {
			r.Notifier.Notify(ctx, ws, run, event)
		}
	}
	return ctrl.Result{}, nil
}

func (r *RunReconciler) event(run *infrav1alpha1.Run, ws *infrav1alpha1.Workspace, eventType, reason, action, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	var related runtime.Object
	if ws != nil {
		related = ws
	}
	r.Recorder.Eventf(run, related, eventType, reason, action, note, args...)
}

// observeTransition records the metrics of a saved phase transition.
func (r *RunReconciler) observeTransition(ctx context.Context, before, run *infrav1alpha1.Run) {
	var step runner.Step
	switch before.Status.Phase {
	case infrav1alpha1.PhasePlanning:
		step = runner.StepPlan
	case infrav1alpha1.PhaseApplying:
		step = runner.StepApply
	}
	if step != "" {
		job := &batchv1.Job{}
		err := r.Get(ctx, types.NamespacedName{Namespace: run.Namespace, Name: stepJobName(run.Name, step)}, job)
		if err == nil && job.Status.StartTime != nil && job.Status.CompletionTime != nil {
			result := "success"
			if run.Status.Phase == infrav1alpha1.PhaseFailed {
				result = "failure"
			}
			duration := job.Status.CompletionTime.Sub(job.Status.StartTime.Time)
			metrics.StepDuration.WithLabelValues(string(step), result).Observe(duration.Seconds())
		}
	}
	if run.Status.Phase.IsFinished() {
		metrics.RunsTotal.WithLabelValues(run.Namespace, run.Spec.WorkspaceRef.Name,
			string(run.Spec.Operation), string(run.Status.Phase)).Inc()
	}
}

// supersedeRequested returns the reason a Run should be superseded, if any.
func supersedeRequested(run *infrav1alpha1.Run) (string, bool) {
	reason, ok := run.Annotations[infrav1alpha1.SupersedeAnnotation]
	if ok && reason == "" {
		reason = "superseded"
	}
	return reason, ok
}

// planDescription is a one-line summary of a plan.
func planDescription(plan *infrav1alpha1.PlanSummary) string {
	if plan == nil || !plan.HasChanges {
		return "no changes"
	}
	desc := fmt.Sprintf("%d to add, %d to change, %d to destroy", plan.Add, plan.Change, plan.Destroy)
	if plan.Import > 0 {
		desc += fmt.Sprintf(", %d to import", plan.Import)
	}
	return desc
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// SetupWithManager sets up the controller with the Manager.
func (r *RunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Run{}).
		Owns(&batchv1.Job{}).
		Named("run").
		Complete(r)
}
