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
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

var testNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

func testRun(n int, generation int64, revision string, phase infrav1alpha1.RunPhase) infrav1alpha1.Run {
	run := infrav1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:   newRunName("ws", n),
			Labels: map[string]string{infrav1alpha1.RunNumberLabel: strconv.Itoa(n)},
		},
		Spec: infrav1alpha1.RunSpec{
			Operation:           infrav1alpha1.OperationApply,
			WorkspaceGeneration: generation,
			Revision:            revision,
		},
		Status: infrav1alpha1.RunStatus{Phase: phase},
	}
	if phase.IsFinished() {
		run.Status.CompletedAt = &metav1.Time{Time: testNow.Add(-10 * time.Minute)}
	}
	return run
}

// stale marks a Run as failed because its plan became stale.
func stale(run infrav1alpha1.Run, trigger infrav1alpha1.RunTrigger) infrav1alpha1.Run {
	run.Spec.Trigger = trigger
	run.Status.FailureReason = infrav1alpha1.FailureStalePlan
	return run
}

func testWorkspace(generation int64) *infrav1alpha1.Workspace {
	return &infrav1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws", Namespace: "default", Generation: generation},
	}
}

func TestNextTrigger(t *testing.T) {
	applied := testRun(1, 1, "rev1", infrav1alpha1.PhaseApplied)
	for name, tc := range map[string]struct {
		mutate   func(*infrav1alpha1.Workspace)
		runs     []infrav1alpha1.Run
		revision string
		inputs   string
		want     infrav1alpha1.RunTrigger
	}{
		"first run":         {runs: nil, revision: "rev1", want: infrav1alpha1.TriggerSpecChange},
		"up to date":        {runs: []infrav1alpha1.Run{applied}, revision: "rev1", want: ""},
		"spec changed":      {mutate: func(ws *infrav1alpha1.Workspace) { ws.Generation = 2 }, runs: []infrav1alpha1.Run{applied}, revision: "rev1", want: infrav1alpha1.TriggerSpecChange},
		"new commit":        {runs: []infrav1alpha1.Run{applied}, revision: "rev2", want: infrav1alpha1.TriggerSourceChange},
		"variables changed": {runs: []infrav1alpha1.Run{applied}, revision: "rev1", inputs: "sha256:new", want: infrav1alpha1.TriggerInputChange},
		"manual": {
			mutate: func(ws *infrav1alpha1.Workspace) {
				ws.Annotations = map[string]string{infrav1alpha1.RequestedAtAnnotation: "now"}
			},
			runs: []infrav1alpha1.Run{applied}, revision: "rev1", want: infrav1alpha1.TriggerManual,
		},
		"manual already handled": {
			mutate: func(ws *infrav1alpha1.Workspace) {
				ws.Annotations = map[string]string{infrav1alpha1.RequestedAtAnnotation: "now"}
				ws.Status.LastHandledRequestedAt = "now"
			},
			runs: []infrav1alpha1.Run{applied}, revision: "rev1", want: "",
		},
		"interval elapsed": {
			mutate: func(ws *infrav1alpha1.Workspace) { ws.Spec.Interval = &metav1.Duration{Duration: 5 * time.Minute} },
			runs:   []infrav1alpha1.Run{applied}, revision: "rev1", want: infrav1alpha1.TriggerInterval,
		},
		"interval not elapsed": {
			mutate: func(ws *infrav1alpha1.Workspace) { ws.Spec.Interval = &metav1.Duration{Duration: time.Hour} },
			runs:   []infrav1alpha1.Run{applied}, revision: "rev1", want: "",
		},
		"active run is the reference": {
			runs: []infrav1alpha1.Run{applied, testRun(2, 1, "rev2", infrav1alpha1.PhasePlanning)}, revision: "rev2", want: "",
		},
		"superseded run is replaced": {
			runs: []infrav1alpha1.Run{applied, testRun(2, 1, "rev2", infrav1alpha1.PhaseSuperseded)}, revision: "rev2",
			want: infrav1alpha1.TriggerSourceChange,
		},
		"failed run is not retried": {
			runs: []infrav1alpha1.Run{applied, testRun(2, 1, "rev2", infrav1alpha1.PhaseFailed)}, revision: "rev2", want: "",
		},
		"stale plan is retried": {
			runs:     []infrav1alpha1.Run{applied, stale(testRun(2, 1, "rev2", infrav1alpha1.PhaseFailed), infrav1alpha1.TriggerSourceChange)},
			revision: "rev2", want: infrav1alpha1.TriggerRetry,
		},
		"stale plan is retried once": {
			runs:     []infrav1alpha1.Run{applied, stale(testRun(2, 1, "rev2", infrav1alpha1.PhaseFailed), infrav1alpha1.TriggerRetry)},
			revision: "rev2", want: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws := testWorkspace(1)
			if tc.mutate != nil {
				tc.mutate(ws)
			}
			if got := nextTrigger(ws, tc.runs, tc.revision, tc.inputs, testNow); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestNewRunName(t *testing.T) {
	if got := newRunName("vpc-prod", 12); got != "vpc-prod-12" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("a", 60) + "-bcd"
	name := newRunName(long, 1234)
	if len(name) > maxNameLength || !strings.HasSuffix(name, "-1234") || strings.Contains(name, "--") {
		t.Fatalf("unexpected name %q (%d characters)", name, len(name))
	}
}

func TestNewRun(t *testing.T) {
	ws := testWorkspace(3)
	run := newRun(ws, []infrav1alpha1.Run{testRun(1, 1, "a", infrav1alpha1.PhaseApplied), testRun(7, 2, "b", infrav1alpha1.PhaseFailed)},
		infrav1alpha1.TriggerSpecChange, "rev", "sha256:inputs")
	if run.Spec.InputsRevision != "sha256:inputs" {
		t.Fatalf("unexpected inputs revision %q", run.Spec.InputsRevision)
	}
	if run.Name != "ws-8" || run.Labels[infrav1alpha1.RunNumberLabel] != "8" || run.Labels[infrav1alpha1.WorkspaceLabel] != "ws" {
		t.Fatalf("unexpected metadata %+v", run.ObjectMeta)
	}
	if run.Spec.WorkspaceGeneration != 3 || run.Spec.Revision != "rev" || !run.Spec.AutoApprove || run.Spec.Operation != infrav1alpha1.OperationApply {
		t.Fatalf("unexpected spec %+v", run.Spec)
	}

	ws.Spec.ApprovalMode = infrav1alpha1.ApprovalManual
	if run := newRun(ws, nil, infrav1alpha1.TriggerSpecChange, "rev", ""); run.Name != "ws-1" || run.Spec.AutoApprove {
		t.Fatalf("unexpected run %s, autoApprove %v", run.Name, run.Spec.AutoApprove)
	}
	if run := newRun(ws, nil, infrav1alpha1.TriggerDeletion, "rev", ""); run.Spec.Operation != infrav1alpha1.OperationDestroy || !run.Spec.AutoApprove {
		t.Fatalf("a deletion run must be an approved destroy, got %+v", run.Spec)
	}
}

func TestRunsToDelete(t *testing.T) {
	runs := []infrav1alpha1.Run{
		testRun(1, 1, "a", infrav1alpha1.PhaseApplied),
		testRun(2, 1, "b", infrav1alpha1.PhaseFailed),
		testRun(3, 1, "c", infrav1alpha1.PhaseApplied),
		testRun(4, 1, "d", infrav1alpha1.PhaseAwaitingApproval),
	}
	deleted := runsToDelete(runs, 2)
	if len(deleted) != 1 || deleted[0].Name != "ws-1" {
		t.Fatalf("unexpected runs to delete %v", deleted)
	}
	if deleted := runsToDelete(runs, 3); len(deleted) != 0 {
		t.Fatalf("nothing should be deleted, got %v", deleted)
	}
}

func TestSortRuns(t *testing.T) {
	runs := []infrav1alpha1.Run{testRun(10, 1, "", ""), testRun(2, 1, "", ""), testRun(9, 1, "", "")}
	sortRuns(runs)
	if runs[0].Name != "ws-2" || runs[2].Name != "ws-10" {
		t.Fatalf("runs are not ordered by number: %s, %s, %s", runs[0].Name, runs[1].Name, runs[2].Name)
	}
}

func TestComputeStatus(t *testing.T) {
	ready := func(ws *infrav1alpha1.Workspace) *metav1.Condition {
		return meta.FindStatusCondition(ws.Status.Conditions, infrav1alpha1.ConditionReady)
	}
	applied := testRun(1, 1, "rev1", infrav1alpha1.PhaseApplied)

	ws := testWorkspace(1)
	computeStatus(ws, []infrav1alpha1.Run{applied}, "rev1")
	if c := ready(ws); c.Status != metav1.ConditionTrue || c.Reason != infrav1alpha1.ReasonSucceeded {
		t.Fatalf("expected Ready, got %+v", c)
	}
	if ws.Status.LastAppliedRevision != "rev1" || ws.Status.LastRun.Name != "ws-1" || ws.Status.CurrentRun != "" {
		t.Fatalf("unexpected status %+v", ws.Status)
	}

	waiting := testRun(2, 1, "rev2", infrav1alpha1.PhaseAwaitingApproval)
	waiting.Status.Plan = &infrav1alpha1.PlanSummary{HasChanges: true, Add: 2}
	computeStatus(ws, []infrav1alpha1.Run{applied, waiting}, "rev2")
	if c := ready(ws); c.Reason != infrav1alpha1.ReasonApprovalRequired || !strings.Contains(c.Message, "2 to add") {
		t.Fatalf("expected ApprovalRequired, got %+v", c)
	}
	if ws.Status.CurrentRun != "ws-2" {
		t.Fatalf("unexpected current run %q", ws.Status.CurrentRun)
	}

	failed := testRun(2, 1, "rev2", infrav1alpha1.PhaseFailed)
	failed.Status.Message = "Error: boom\nmore details"
	computeStatus(ws, []infrav1alpha1.Run{applied, failed}, "rev2")
	if c := ready(ws); c.Reason != infrav1alpha1.ReasonRunFailed || c.Message != "run ws-2 failed: Error: boom" {
		t.Fatalf("expected RunFailed, got %+v", c)
	}
	if ws.Status.LastAppliedRevision != "rev1" {
		t.Fatalf("a failed run must not change the applied revision, got %q", ws.Status.LastAppliedRevision)
	}

	destroyed := testRun(3, 1, "rev2", infrav1alpha1.PhaseApplied)
	destroyed.Spec.Operation = infrav1alpha1.OperationDestroy
	computeStatus(ws, []infrav1alpha1.Run{applied, destroyed}, "rev2")
	if ws.Status.LastAppliedRevision != "rev1" {
		t.Fatalf("a destroy must not change the applied revision, got %q", ws.Status.LastAppliedRevision)
	}

	ws.Spec.Suspend = true
	computeStatus(ws, []infrav1alpha1.Run{applied}, "rev1")
	if c := ready(ws); c.Reason != infrav1alpha1.ReasonSuspended {
		t.Fatalf("expected Suspended, got %+v", c)
	}
}

func TestInputRefs(t *testing.T) {
	ws := testWorkspace(1)
	ws.Spec.Vars = []infrav1alpha1.Variable{
		{Name: "a", ValueFrom: &infrav1alpha1.VariableSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password", Optional: new(true),
		}}},
		{Name: "b", ValueFrom: &infrav1alpha1.VariableSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "common"}, Key: "region",
		}}},
	}
	ws.Spec.VarsFrom = []infrav1alpha1.VarsFromSource{{Kind: "Secret", Name: "db"}, {Kind: "Secret", Name: "extra", Optional: true}}

	got := inputRefs(ws)
	want := []inputRef{{"ConfigMap", "common", false}, {"Secret", "db", false}, {"Secret", "extra", true}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if inputsRevision(nil) != "" || inputsRevision([]string{"a=1"}) == inputsRevision([]string{"a=2"}) {
		t.Fatal("unexpected inputs revisions")
	}
}

func TestValidateWorkspace(t *testing.T) {
	ws := testWorkspace(1)
	ws.Spec.Vars = []infrav1alpha1.Variable{{Name: "ok", ValueFrom: &infrav1alpha1.VariableSource{}}, {Name: "missing"}}
	err := validateWorkspace(ws)
	if err == nil || !strings.Contains(err.Error(), "variable missing") || strings.Contains(err.Error(), "variable ok") {
		t.Fatalf("unexpected validation error %v", err)
	}
}
