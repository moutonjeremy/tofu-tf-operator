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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/metrics"
	"github.com/labbs/tofu-tf-operator/internal/runner"
	"github.com/labbs/tofu-tf-operator/internal/source"
)

// recordingNotifier records the notifications as "run:event".
type recordingNotifier struct {
	mu     sync.Mutex
	events []string
}

func (n *recordingNotifier) Notify(_ context.Context, _ *infrav1alpha1.Workspace, run *infrav1alpha1.Run, event infrav1alpha1.NotificationEvent) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, run.Name+":"+string(event))
}

func (n *recordingNotifier) Events() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.events...)
}

// These tests drive both reconcilers against a real API server. There is no Job
// controller nor kubelet in envtest, so finishJob plays their part.
var _ = Describe("Workspace lifecycle", func() {
	const module = `resource "terraform_data" "this" {}`

	var (
		namespace     string
		notifier      *recordingNotifier
		wsReconciler  *WorkspaceReconciler
		runReconciler *RunReconciler
	)

	BeforeEach(func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "lifecycle-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		namespace = ns.Name
		notifier = &recordingNotifier{}
		wsReconciler = &WorkspaceReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		runReconciler = &RunReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), RunnerImage: "runner:test", Notifier: notifier}
	})

	key := func(name string) types.NamespacedName { return types.NamespacedName{Namespace: namespace, Name: name} }

	reconcileWorkspace := func(name string) {
		GinkgoHelper()
		_, err := wsReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		Expect(err).NotTo(HaveOccurred())
	}
	reconcileRun := func(name string) {
		GinkgoHelper()
		_, err := runReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key(name)})
		Expect(err).NotTo(HaveOccurred())
	}
	getWorkspace := func(name string) *infrav1alpha1.Workspace {
		GinkgoHelper()
		ws := &infrav1alpha1.Workspace{}
		Expect(k8sClient.Get(ctx, key(name), ws)).To(Succeed())
		return ws
	}
	getRun := func(name string) *infrav1alpha1.Run {
		GinkgoHelper()
		run := &infrav1alpha1.Run{}
		Expect(k8sClient.Get(ctx, key(name), run)).To(Succeed())
		return run
	}
	listRuns := func() []string {
		GinkgoHelper()
		list := &infrav1alpha1.RunList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(namespace))).To(Succeed())
		names := make([]string, 0, len(list.Items))
		for _, run := range list.Items {
			names = append(names, run.Name)
		}
		return names
	}
	ready := func(name string) *metav1.Condition {
		GinkgoHelper()
		return meta.FindStatusCondition(getWorkspace(name).Status.Conditions, infrav1alpha1.ConditionReady)
	}

	createWorkspace := func(name string, mutate func(*infrav1alpha1.Workspace)) {
		GinkgoHelper()
		ws := &infrav1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec:       infrav1alpha1.WorkspaceSpec{Source: infrav1alpha1.SourceSpec{Inline: module}},
		}
		if mutate != nil {
			mutate(ws)
		}
		Expect(k8sClient.Create(ctx, ws)).To(Succeed())
		// A single reconciliation adds the finalizer and creates the first Run.
		reconcileWorkspace(name)
		Expect(getWorkspace(name).Finalizers).To(ContainElement(infrav1alpha1.Finalizer))
	}

	// finishJob plays the Job controller and the kubelet: it creates the pod of the
	// Job, terminated with the given message, and marks the Job as finished.
	finishJob := func(name, message string, succeeded bool) {
		GinkgoHelper()
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key(name), job)).To(Succeed())

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-pod", Namespace: namespace, Labels: map[string]string{jobNameLabel: name}},
			Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever,
				Containers:    []corev1.Container{{Name: runnerContainer, Image: "runner:test"}},
			},
		}
		Expect(k8sClient.Create(ctx, pod)).To(Succeed())
		exitCode := int32(0)
		if !succeeded {
			exitCode = 1
		}
		pod.Status.Phase = corev1.PodSucceeded
		if !succeeded {
			pod.Status.Phase = corev1.PodFailed
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  runnerContainer,
			Image: "runner:test",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode, Message: message}},
		}}
		Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

		now := metav1.Now()
		job.Status.StartTime = &now
		if succeeded {
			job.Status.Succeeded = 1
			job.Status.CompletionTime = &now
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
			}
		} else {
			job.Status.Failed = 1
			job.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded, LastTransitionTime: now},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonBackoffLimitExceeded,
					Message: "Job has reached the specified backoff limit", LastTransitionTime: now},
			}
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	planDigest := func(add, destroy int32) string { return runner.Digest(fmt.Appendf(nil, "%d-%d", add, destroy)) }
	planResult := func(add, destroy int32) string {
		summary := &infrav1alpha1.PlanSummary{HasChanges: add+destroy > 0, Add: add, Destroy: destroy}
		if summary.HasChanges {
			summary.Digest = planDigest(add, destroy)
		}
		return string(runner.Result{Step: runner.StepPlan, Revision: source.InlineRevision(module), Plan: summary}.Encode())
	}
	applyResult := string(runner.Result{Step: runner.StepApply}.Encode())

	// applyRun drives an auto-approved Run through a plan with changes and a successful apply.
	applyRun := func(name string, add, destroy int32) {
		GinkgoHelper()
		reconcileRun(name)
		Expect(getRun(name).Status.Phase).To(Equal(infrav1alpha1.PhasePlanning))
		finishJob(stepJobName(name, runner.StepPlan), planResult(add, destroy), true)
		reconcileRun(name)
		Expect(getRun(name).Status.Phase).To(Equal(infrav1alpha1.PhaseApplying))
		finishJob(stepJobName(name, runner.StepApply), applyResult, true)
		reconcileRun(name)
		Expect(getRun(name).Status.Phase).To(Equal(infrav1alpha1.PhaseApplied))
	}

	It("plans and applies automatically", func() {
		createWorkspace("auto", nil)
		Expect(listRuns()).To(ConsistOf("auto-1"))
		ws := getWorkspace("auto")
		Expect(ws.Status.CurrentRun).To(Equal("auto-1"))
		Expect(ws.Status.SourceRevision).To(Equal(source.InlineRevision(module)))

		By("creating the runner ServiceAccount and its permissions")
		Expect(k8sClient.Get(ctx, key(DefaultRunnerServiceAccount), &corev1.ServiceAccount{})).To(Succeed())
		Expect(k8sClient.Get(ctx, key(DefaultRunnerServiceAccount), &rbacv1.Role{})).To(Succeed())
		Expect(k8sClient.Get(ctx, key(DefaultRunnerServiceAccount), &rbacv1.RoleBinding{})).To(Succeed())

		By("running the plan, then the apply with the planned digest")
		reconcileRun("auto-1")
		finishJob("auto-1-plan", planResult(1, 0), true)
		reconcileRun("auto-1")
		run := getRun("auto-1")
		Expect(run.Status.Phase).To(Equal(infrav1alpha1.PhaseApplying))
		Expect(run.Status.Plan.Add).To(BeEquivalentTo(1))
		apply := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key("auto-1-apply"), apply)).To(Succeed())
		Expect(apply.Spec.Template.Spec.Containers[0].Env).To(ContainElement(corev1.EnvVar{Name: runner.EnvPlanDigest, Value: planDigest(1, 0)}))

		finishJob("auto-1-apply", applyResult, true)
		reconcileRun("auto-1")
		run = getRun("auto-1")
		Expect(run.Status.Phase).To(Equal(infrav1alpha1.PhaseApplied))
		Expect(run.Status.CompletedAt).NotTo(BeNil())
		Expect(notifier.Events()).To(Equal([]string{"auto-1:PlanHasChanges", "auto-1:ApplySucceeded"}))

		By("reporting the Workspace as ready, without a new Run")
		reconcileWorkspace("auto")
		Expect(ready("auto").Status).To(Equal(metav1.ConditionTrue))
		Expect(testutil.ToFloat64(metrics.WorkspaceReady.WithLabelValues(namespace, "auto", infrav1alpha1.ReasonSucceeded))).To(Equal(1.0))
		Expect(testutil.ToFloat64(metrics.RunsTotal.WithLabelValues(namespace, "auto", "Apply", "Applied"))).To(Equal(1.0))
		Expect(getWorkspace("auto").Status.LastAppliedRevision).To(Equal(source.InlineRevision(module)))
		reconcileWorkspace("auto")
		Expect(listRuns()).To(ConsistOf("auto-1"))

		By("finishing without apply when the plan has no changes")
		ws = getWorkspace("auto")
		ws.Annotations = map[string]string{infrav1alpha1.RequestedAtAnnotation: "1"}
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		reconcileWorkspace("auto")
		Expect(getRun("auto-2").Spec.Trigger).To(Equal(infrav1alpha1.TriggerManual))
		reconcileRun("auto-2")
		finishJob("auto-2-plan", planResult(0, 0), true)
		reconcileRun("auto-2")
		Expect(getRun("auto-2").Status.Phase).To(Equal(infrav1alpha1.PhaseNoChanges))
		Expect(k8sClient.Get(ctx, key("auto-2-apply"), &batchv1.Job{})).NotTo(Succeed())
	})

	It("waits for approval and supersedes outdated plans", func() {
		createWorkspace("manual", func(ws *infrav1alpha1.Workspace) { ws.Spec.ApprovalMode = infrav1alpha1.ApprovalManual })
		Expect(getRun("manual-1").Spec.AutoApprove).To(BeFalse())

		reconcileRun("manual-1")
		finishJob("manual-1-plan", planResult(2, 0), true)
		reconcileRun("manual-1")
		Expect(getRun("manual-1").Status.Phase).To(Equal(infrav1alpha1.PhaseAwaitingApproval))
		Expect(notifier.Events()).To(ContainElement("manual-1:ApprovalRequired"))
		reconcileWorkspace("manual")
		Expect(ready("manual").Reason).To(Equal(infrav1alpha1.ReasonApprovalRequired))

		By("superseding the pending Run when the spec changes")
		ws := getWorkspace("manual")
		ws.Spec.Source.Inline = module + "\n# changed\n"
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		reconcileWorkspace("manual")
		Expect(getRun("manual-1").Annotations).To(HaveKeyWithValue(infrav1alpha1.SupersedeAnnotation, "SpecChange"))
		Expect(listRuns()).To(ConsistOf("manual-1"))
		reconcileRun("manual-1")
		Expect(getRun("manual-1").Status.Phase).To(Equal(infrav1alpha1.PhaseSuperseded))
		reconcileWorkspace("manual")
		Expect(getRun("manual-2").Spec.Trigger).To(Equal(infrav1alpha1.TriggerSpecChange))

		By("applying once approved")
		reconcileRun("manual-2")
		finishJob("manual-2-plan", planResult(2, 0), true)
		reconcileRun("manual-2")
		Expect(getRun("manual-2").Status.Phase).To(Equal(infrav1alpha1.PhaseAwaitingApproval))
		reconcileRun("manual-2")
		Expect(getRun("manual-2").Status.Phase).To(Equal(infrav1alpha1.PhaseAwaitingApproval))

		Expect(getRun("manual-2").Status.Message).To(ContainSubstring(`"approvedPlan":"` + planDigest(2, 0) + `"`))

		By("refusing the approval of another plan")
		run := getRun("manual-2")
		run.Spec.ApprovedPlan = planDigest(3, 0)
		Expect(k8sClient.Update(ctx, run)).To(Succeed())
		reconcileRun("manual-2")
		Expect(getRun("manual-2").Status.Phase).To(Equal(infrav1alpha1.PhaseAwaitingApproval))
		Expect(getRun("manual-2").Status.Message).To(ContainSubstring("is not the digest of the plan"))

		run = getRun("manual-2")
		run.Spec.ApprovedPlan = planDigest(2, 0)
		Expect(k8sClient.Update(ctx, run)).To(Succeed())
		reconcileRun("manual-2")
		Expect(getRun("manual-2").Status.Phase).To(Equal(infrav1alpha1.PhaseApplying))
		finishJob("manual-2-apply", applyResult, true)
		reconcileRun("manual-2")
		Expect(getRun("manual-2").Status.Phase).To(Equal(infrav1alpha1.PhaseApplied))
		reconcileWorkspace("manual")
		Expect(ready("manual").Status).To(Equal(metav1.ConditionTrue))
	})

	It("reports a failed plan and retries on request", func() {
		createWorkspace("failing", nil)
		reconcileRun("failing-1")
		finishJob("failing-1-plan", "panic: runtime error\ngoroutine 1 [running]", false)
		reconcileRun("failing-1")

		run := getRun("failing-1")
		Expect(run.Status.Phase).To(Equal(infrav1alpha1.PhaseFailed))
		Expect(run.Status.Message).To(ContainSubstring("the runner failed"))
		Expect(run.Status.Message).To(ContainSubstring("panic: runtime error"))
		Expect(notifier.Events()).To(Equal([]string{"failing-1:PlanFailed"}))

		reconcileWorkspace("failing")
		Expect(ready("failing").Reason).To(Equal(infrav1alpha1.ReasonRunFailed))
		Expect(testutil.ToFloat64(metrics.WorkspaceReady.WithLabelValues(namespace, "failing", infrav1alpha1.ReasonRunFailed))).To(Equal(0.0))
		Expect(testutil.ToFloat64(metrics.RunsTotal.WithLabelValues(namespace, "failing", "Apply", "Failed"))).To(Equal(1.0))
		Expect(listRuns()).To(ConsistOf("failing-1"))

		ws := getWorkspace("failing")
		ws.Annotations = map[string]string{infrav1alpha1.RequestedAtAnnotation: "retry"}
		Expect(k8sClient.Update(ctx, ws)).To(Succeed())
		reconcileWorkspace("failing")
		Expect(listRuns()).To(ConsistOf("failing-1", "failing-2"))
		Expect(getWorkspace("failing").Status.LastHandledRequestedAt).To(Equal("retry"))
	})

	It("reports the error written by the runner", func() {
		createWorkspace("error", nil)
		reconcileRun("error-1")
		result := runner.Result{Step: runner.StepPlan, Error: "tofu plan: exit code 1", LogTail: "Error: Unsupported argument\n"}
		finishJob("error-1-plan", string(result.Encode()), false)
		reconcileRun("error-1")
		Expect(getRun("error-1").Status.Message).To(Equal("tofu plan: exit code 1\nError: Unsupported argument"))
	})

	It("plans again when a Secret of input variables changes", func() {
		createWorkspace("inputs", func(ws *infrav1alpha1.Workspace) {
			ws.Spec.VarsFrom = []infrav1alpha1.VarsFromSource{{Kind: "Secret", Name: "tfvars"}}
		})
		Expect(listRuns()).To(BeEmpty(), "no run can start without its required Secret")
		Expect(ready("inputs").Reason).To(Equal(infrav1alpha1.ReasonInputsFailed))

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "tfvars", Namespace: namespace},
			StringData: map[string]string{"region": "eu-west-3"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		reconcileWorkspace("inputs")
		Expect(getRun("inputs-1").Spec.InputsRevision).NotTo(BeEmpty())
		applyRun("inputs-1", 1, 0)
		reconcileWorkspace("inputs")
		Expect(listRuns()).To(ConsistOf("inputs-1"))

		secret.StringData = map[string]string{"region": "eu-west-1"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		reconcileWorkspace("inputs")
		Expect(getRun("inputs-2").Spec.Trigger).To(Equal(infrav1alpha1.TriggerInputChange))
	})

	It("plans again once when the plan became stale", func() {
		createWorkspace("stale", nil)
		reconcileRun("stale-1")
		finishJob("stale-1-plan", planResult(1, 0), true)
		reconcileRun("stale-1")
		staleResult := runner.Result{Step: runner.StepApply, Error: "tofu apply: exit code 1: Saved plan is stale", StalePlan: true}
		finishJob("stale-1-apply", string(staleResult.Encode()), false)
		reconcileRun("stale-1")
		Expect(getRun("stale-1").Status.FailureReason).To(Equal(infrav1alpha1.FailureStalePlan))

		reconcileWorkspace("stale")
		Expect(getRun("stale-2").Spec.Trigger).To(Equal(infrav1alpha1.TriggerRetry))
	})

	It("reports a timeout", func() {
		createWorkspace("slow", nil)
		reconcileRun("slow-1")
		job := &batchv1.Job{}
		Expect(k8sClient.Get(ctx, key("slow-1-plan"), job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: batchv1.JobReasonDeadlineExceeded, LastTransitionTime: now},
		}
		Expect(k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileRun("slow-1")
		run := getRun("slow-1")
		Expect(run.Status.Phase).To(Equal(infrav1alpha1.PhaseFailed))
		Expect(run.Status.FailureReason).To(Equal(infrav1alpha1.FailureTimeout))
		Expect(run.Status.Message).To(ContainSubstring("exceeded its timeout"))
	})

	It("destroys the resources on deletion", func() {
		createWorkspace("destroy", func(ws *infrav1alpha1.Workspace) { ws.Spec.DestroyOnDelete = true })
		applyRun("destroy-1", 1, 0)
		reconcileWorkspace("destroy")

		Expect(k8sClient.Delete(ctx, getWorkspace("destroy"))).To(Succeed())
		reconcileWorkspace("destroy")
		run := getRun("destroy-2")
		Expect(run.Spec.Operation).To(Equal(infrav1alpha1.OperationDestroy))
		Expect(run.Spec.Trigger).To(Equal(infrav1alpha1.TriggerDeletion))
		Expect(run.Spec.AutoApprove).To(BeTrue())
		Expect(ready("destroy").Reason).To(Equal(infrav1alpha1.ReasonDeleting))

		applyRun("destroy-2", 0, 1)
		Expect(notifier.Events()).To(ContainElement("destroy-2:DestroySucceeded"))
		reconcileWorkspace("destroy")
		err := k8sClient.Get(ctx, key("destroy"), &infrav1alpha1.Workspace{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Workspace should be gone, got %v", err)
		Expect(metrics.WorkspaceReady.DeletePartialMatch(prometheus.Labels{"namespace": namespace})).To(BeZero(),
			"the series of a deleted Workspace must be removed")
	})

	It("detects drift at the interval", func() {
		createWorkspace("drift", func(ws *infrav1alpha1.Workspace) {
			ws.Spec.Interval = &metav1.Duration{Duration: 10 * time.Minute}
		})
		applyRun("drift-1", 1, 0)
		reconcileWorkspace("drift")
		Expect(listRuns()).To(ConsistOf("drift-1"))

		By("planning again once the interval has elapsed")
		wsReconciler.Now = func() time.Time { return time.Now().Add(11 * time.Minute) }
		reconcileWorkspace("drift")
		Expect(getRun("drift-2").Spec.Trigger).To(Equal(infrav1alpha1.TriggerInterval))
		applyRun("drift-2", 1, 1)
		Expect(notifier.Events()).To(ContainElement("drift-2:DriftDetected"))
		Expect(testutil.ToFloat64(metrics.DriftDetectedTotal.WithLabelValues(namespace, "drift"))).To(Equal(1.0))
	})

	It("does not destroy what was never applied", func() {
		createWorkspace("never", func(ws *infrav1alpha1.Workspace) {
			ws.Spec.DestroyOnDelete = true
			ws.Spec.ApprovalMode = infrav1alpha1.ApprovalManual
		})
		Expect(k8sClient.Delete(ctx, getWorkspace("never"))).To(Succeed())
		reconcileWorkspace("never")
		err := k8sClient.Get(ctx, key("never"), &infrav1alpha1.Workspace{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the Workspace should be gone, got %v", err)
	})
})
