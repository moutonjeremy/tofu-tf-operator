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
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// This file holds the decisions of the Workspace controller. They are pure
// functions of the Workspace and its Runs, so they can be tested without a cluster.

// runNumber returns the position of a Run in the history of its Workspace.
func runNumber(run *infrav1alpha1.Run) int {
	n, _ := strconv.Atoi(run.Labels[infrav1alpha1.RunNumberLabel])
	return n
}

// sortRuns orders Runs from the oldest to the most recent.
func sortRuns(runs []infrav1alpha1.Run) {
	slices.SortFunc(runs, func(a, b infrav1alpha1.Run) int {
		return cmp.Or(
			cmp.Compare(runNumber(&a), runNumber(&b)),
			a.CreationTimestamp.Compare(b.CreationTimestamp.Time),
			cmp.Compare(a.Name, b.Name),
		)
	})
}

// activeRun returns the Run that is not finished, if any.
func activeRun(runs []infrav1alpha1.Run) *infrav1alpha1.Run {
	for i := len(runs) - 1; i >= 0; i-- {
		if !runs[i].Status.Phase.IsFinished() {
			return &runs[i]
		}
	}
	return nil
}

// lastRun returns the most recent Run matching a predicate.
func lastRun(runs []infrav1alpha1.Run, match func(*infrav1alpha1.Run) bool) *infrav1alpha1.Run {
	for i := len(runs) - 1; i >= 0; i-- {
		if match(&runs[i]) {
			return &runs[i]
		}
	}
	return nil
}

func notSuperseded(run *infrav1alpha1.Run) bool {
	return run.Status.Phase != infrav1alpha1.PhaseSuperseded
}

// cancellable tells whether a Run can still be superseded without interrupting a Job.
func cancellable(run *infrav1alpha1.Run) bool {
	switch run.Status.Phase {
	case "", infrav1alpha1.PhasePending, infrav1alpha1.PhaseAwaitingApproval:
		return true
	}
	return false
}

// nextTrigger tells why a new Run is needed, if it is. The reference is the last
// Run that was not superseded, so a superseded Run is replaced.
func nextTrigger(ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run, revision, inputs string, now time.Time) infrav1alpha1.RunTrigger {
	last := lastRun(runs, notSuperseded)
	requested := ws.Annotations[infrav1alpha1.RequestedAtAnnotation]
	switch {
	case last == nil, ws.Generation != last.Spec.WorkspaceGeneration:
		return infrav1alpha1.TriggerSpecChange
	case revision != last.Spec.Revision:
		return infrav1alpha1.TriggerSourceChange
	case inputs != last.Spec.InputsRevision:
		return infrav1alpha1.TriggerInputChange
	case requested != "" && requested != ws.Status.LastHandledRequestedAt:
		return infrav1alpha1.TriggerManual
	case last.Status.FailureReason == infrav1alpha1.FailureStalePlan && last.Spec.Trigger != infrav1alpha1.TriggerRetry:
		// Plan again once: the state changed outside of this Workspace.
		return infrav1alpha1.TriggerRetry
	}
	if due, ok := nextIntervalRun(ws, last); ok && !now.Before(due) {
		return infrav1alpha1.TriggerInterval
	}
	return ""
}

// nextIntervalRun returns when the drift detection is due after the last Run.
func nextIntervalRun(ws *infrav1alpha1.Workspace, last *infrav1alpha1.Run) (time.Time, bool) {
	if ws.Spec.Interval == nil || ws.Spec.Interval.Duration <= 0 || last == nil || last.Status.CompletedAt == nil {
		return time.Time{}, false
	}
	return last.Status.CompletedAt.Add(ws.Spec.Interval.Duration), true
}

// newRunName returns the name of the n-th Run of a Workspace. Names are
// deterministic, so a Run created from a stale cache fails as AlreadyExists.
func newRunName(wsName string, n int) string {
	suffix := "-" + strconv.Itoa(n)
	prefix := wsName
	if len(prefix)+len(suffix) > maxNameLength {
		prefix = strings.TrimRight(prefix[:maxNameLength-len(suffix)], "-.")
	}
	return prefix + suffix
}

// newRun returns the next Run of a Workspace.
func newRun(ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run, trigger infrav1alpha1.RunTrigger, revision, inputs string) *infrav1alpha1.Run {
	n := 1
	if len(runs) > 0 {
		n = runNumber(&runs[len(runs)-1]) + 1
	}
	operation := infrav1alpha1.OperationApply
	autoApprove := ws.Spec.ApprovalMode != infrav1alpha1.ApprovalManual
	if trigger == infrav1alpha1.TriggerDeletion {
		// Deleting a Workspace with destroyOnDelete is the approval.
		operation, autoApprove = infrav1alpha1.OperationDestroy, true
	}
	return &infrav1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newRunName(ws.Name, n),
			Namespace: ws.Namespace,
			Labels: map[string]string{
				infrav1alpha1.WorkspaceLabel: ws.Name,
				infrav1alpha1.RunNumberLabel: strconv.Itoa(n),
			},
		},
		Spec: infrav1alpha1.RunSpec{
			WorkspaceRef:        corev1.LocalObjectReference{Name: ws.Name},
			Operation:           operation,
			Trigger:             trigger,
			Revision:            revision,
			InputsRevision:      inputs,
			WorkspaceGeneration: ws.Generation,
			AutoApprove:         autoApprove,
		},
	}
}

// runsToDelete returns the finished Runs beyond the history limit, oldest first.
func runsToDelete(runs []infrav1alpha1.Run, limit int32) []*infrav1alpha1.Run {
	var finished []*infrav1alpha1.Run
	for i := range runs {
		if runs[i].Status.Phase.IsFinished() {
			finished = append(finished, &runs[i])
		}
	}
	if excess := len(finished) - int(limit); excess > 0 {
		return finished[:excess]
	}
	return nil
}

// computeStatus updates the run related fields and the Ready condition of a Workspace.
func computeStatus(ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run, revision string) {
	active := activeRun(runs)
	ws.Status.CurrentRun = ""
	if active != nil {
		ws.Status.CurrentRun = active.Name
	}

	last := lastRun(runs, func(r *infrav1alpha1.Run) bool { return r.Status.Phase.IsFinished() && notSuperseded(r) })
	ws.Status.LastRun = nil
	if last != nil {
		ws.Status.LastRun = &infrav1alpha1.RunReference{Name: last.Name, Phase: last.Status.Phase, CompletedAt: last.Status.CompletedAt}
	}
	applied := lastRun(runs, func(r *infrav1alpha1.Run) bool {
		return r.Spec.Operation == infrav1alpha1.OperationApply &&
			(r.Status.Phase == infrav1alpha1.PhaseApplied || r.Status.Phase == infrav1alpha1.PhaseNoChanges)
	})
	if applied != nil {
		ws.Status.LastAppliedRevision = applied.Spec.Revision
		ws.Status.LastAppliedAt = applied.Status.CompletedAt
	}

	status, reason, message := metav1.ConditionFalse, infrav1alpha1.ReasonProgressing, "waiting for a run"
	switch {
	case ws.Spec.Suspend:
		reason, message = infrav1alpha1.ReasonSuspended, "reconciliation is suspended"
	case active != nil && active.Status.Phase == infrav1alpha1.PhaseAwaitingApproval:
		reason = infrav1alpha1.ReasonApprovalRequired
		message = fmt.Sprintf("run %s awaits approval: %s", active.Name, planDescription(active.Status.Plan))
	case active != nil:
		message = fmt.Sprintf("run %s is %s", active.Name, strings.ToLower(string(cmp.Or(active.Status.Phase, infrav1alpha1.PhasePending))))
	case last == nil:
	case last.Status.Phase == infrav1alpha1.PhaseFailed:
		reason, message = infrav1alpha1.ReasonRunFailed, fmt.Sprintf("run %s failed: %s", last.Name, firstLine(last.Status.Message))
	case last.Spec.WorkspaceGeneration == ws.Generation && last.Spec.Revision == revision:
		status, reason = metav1.ConditionTrue, infrav1alpha1.ReasonSucceeded
		message = fmt.Sprintf("run %s: %s", last.Name, strings.ToLower(string(last.Status.Phase)))
	}
	setCondition(ws, infrav1alpha1.ConditionReady, status, reason, message)
}

func setCondition(ws *infrav1alpha1.Workspace, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&ws.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: ws.Generation,
	})
}

// inputRef is a Secret or ConfigMap holding input variables.
type inputRef struct {
	kind, name string
	optional   bool
}

// String is the value of the index of referenced objects.
func (r inputRef) String() string {
	return r.kind + "/" + r.name
}

// inputRefs returns the Secrets and ConfigMaps holding the input variables of a Workspace, sorted.
func inputRefs(ws *infrav1alpha1.Workspace) []inputRef {
	byKey := map[string]inputRef{}
	add := func(ref inputRef) {
		// A reference is optional only if all of its uses are.
		if existing, ok := byKey[ref.String()]; ok {
			ref.optional = ref.optional && existing.optional
		}
		byKey[ref.String()] = ref
	}
	for _, v := range ws.Spec.Vars {
		switch {
		case v.ValueFrom == nil:
		case v.ValueFrom.SecretKeyRef != nil:
			add(inputRef{"Secret", v.ValueFrom.SecretKeyRef.Name, ptr.Deref(v.ValueFrom.SecretKeyRef.Optional, false)})
		case v.ValueFrom.ConfigMapKeyRef != nil:
			add(inputRef{"ConfigMap", v.ValueFrom.ConfigMapKeyRef.Name, ptr.Deref(v.ValueFrom.ConfigMapKeyRef.Optional, false)})
		}
	}
	for _, from := range ws.Spec.VarsFrom {
		add(inputRef{from.Kind, from.Name, from.Optional})
	}
	refs := slices.Collect(maps.Values(byKey))
	slices.SortFunc(refs, func(a, b inputRef) int { return strings.Compare(a.String(), b.String()) })
	return refs
}

// inputsRevision identifies a set of object versions. It only depends on
// metadata, so the values of the Secrets are never read.
func inputsRevision(versions []string) string {
	if len(versions) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(versions, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// validateWorkspace checks what the CRD schema cannot express.
func validateWorkspace(ws *infrav1alpha1.Workspace) error {
	var errs []error
	for _, v := range ws.Spec.Vars {
		if (v.Value == nil) == (v.ValueFrom == nil) {
			errs = append(errs, fmt.Errorf("variable %s: exactly one of value or valueFrom must be set", v.Name))
		}
	}
	return errors.Join(errs...)
}
