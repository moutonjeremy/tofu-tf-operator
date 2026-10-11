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

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

const (
	namespace = "infra"
	digest    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
)

func newClient(t *testing.T) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	ws := &infrav1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: namespace},
		Status:     infrav1alpha1.WorkspaceStatus{CurrentRun: "vpc-3", LastRun: &infrav1alpha1.RunReference{Name: "vpc-2"}},
	}
	idle := &infrav1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "idle", Namespace: namespace},
		Status:     infrav1alpha1.WorkspaceStatus{LastRun: &infrav1alpha1.RunReference{Name: "vpc-2"}},
	}
	waiting := &infrav1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc-3", Namespace: namespace},
		Spec: infrav1alpha1.RunSpec{
			WorkspaceRef: corev1.LocalObjectReference{Name: "vpc"},
			Operation:    infrav1alpha1.OperationApply,
			Trigger:      infrav1alpha1.TriggerSourceChange,
			Revision:     "main@sha1:abc",
		},
		Status: infrav1alpha1.RunStatus{
			Phase:   infrav1alpha1.PhaseAwaitingApproval,
			PlanJob: "vpc-3-plan",
			Plan:    &infrav1alpha1.PlanSummary{HasChanges: true, Add: 2, Change: 1, Digest: digest},
		},
	}
	applied := &infrav1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc-2", Namespace: namespace},
		Status:     infrav1alpha1.RunStatus{Phase: infrav1alpha1.PhaseApplied, PlanJob: "vpc-2-plan", ApplyJob: "vpc-2-apply"},
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(ws, idle, waiting, applied).Build()
}

func TestResolveRun(t *testing.T) {
	c := newClient(t)
	ctx := context.Background()
	for name, want := range map[string]string{"vpc-3": "vpc-3", "vpc": "vpc-3", "idle": "vpc-2"} {
		run, err := ResolveRun(ctx, c, namespace, name)
		if err != nil || run.Name != want {
			t.Errorf("ResolveRun(%s) = %v, %v; want %s", name, run, err, want)
		}
	}
	if _, err := ResolveRun(ctx, c, namespace, "missing"); err == nil || !strings.Contains(err.Error(), "no Run nor Workspace") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestApprove(t *testing.T) {
	ctx := context.Background()

	c := newClient(t)
	var out bytes.Buffer
	if err := Approve(ctx, c, &out, strings.NewReader("n\n"), namespace, "vpc", false); !errors.Is(err, ErrCancelled) {
		t.Fatalf("expected a cancellation, got %v", err)
	}
	if !strings.Contains(out.String(), "Plan: 2 to add, 1 to change, 0 to destroy") || !strings.Contains(out.String(), "logs job/vpc-3-plan") {
		t.Fatalf("the plan must be shown before the confirmation:\n%s", out.String())
	}
	run, _ := ResolveRun(ctx, c, namespace, "vpc-3")
	if run.Spec.ApprovedPlan != "" {
		t.Fatal("a cancelled approval must not change the run")
	}

	if err := Approve(ctx, c, &out, strings.NewReader("yes\n"), namespace, "vpc", false); err != nil {
		t.Fatal(err)
	}
	run, _ = ResolveRun(ctx, c, namespace, "vpc-3")
	if run.Spec.ApprovedPlan != digest {
		t.Fatalf("expected the digest of the plan to be approved, got %q", run.Spec.ApprovedPlan)
	}

	if err := Approve(ctx, c, &out, nil, namespace, "vpc-2", true); err == nil || !strings.Contains(err.Error(), "not awaiting approval") {
		t.Fatalf("an applied run cannot be approved, got %v", err)
	}
}

func TestReconcile(t *testing.T) {
	ctx := context.Background()
	c := newClient(t)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	var out bytes.Buffer
	if err := Reconcile(ctx, c, &out, namespace, "vpc", now); err != nil {
		t.Fatal(err)
	}
	ws := &infrav1alpha1.Workspace{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: "vpc"}, ws); err != nil {
		t.Fatal(err)
	}
	if got := ws.Annotations[infrav1alpha1.RequestedAtAnnotation]; got != "2026-10-11T12:00:00Z" {
		t.Fatalf("unexpected annotation %q", got)
	}
}

func TestStepJob(t *testing.T) {
	run := &infrav1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: "vpc-3"}, Status: infrav1alpha1.RunStatus{PlanJob: "vpc-3-plan"}}
	if job, err := StepJob(run, ""); err != nil || job != "vpc-3-plan" {
		t.Fatalf("got %q, %v", job, err)
	}
	if _, err := StepJob(run, "apply"); err == nil {
		t.Fatal("expected an error for a missing apply step")
	}
	run.Status.ApplyJob = "vpc-3-apply"
	if job, _ := StepJob(run, ""); job != "vpc-3-apply" {
		t.Fatalf("the last step must be the default, got %q", job)
	}
	if _, err := StepJob(run, "destroy"); err == nil {
		t.Fatal("expected an error for an unknown step")
	}
}
