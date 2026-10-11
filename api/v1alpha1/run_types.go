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

package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// RunOperation is what a Run does once its plan is approved.
// +kubebuilder:validation:Enum=Apply;Destroy
type RunOperation string

const (
	OperationApply   RunOperation = "Apply"
	OperationDestroy RunOperation = "Destroy"
)

// RunTrigger is the reason a Run was created.
// +kubebuilder:validation:Enum=SpecChange;SourceChange;InputChange;Interval;Manual;Retry;Deletion
type RunTrigger string

const (
	TriggerSpecChange   RunTrigger = "SpecChange"
	TriggerSourceChange RunTrigger = "SourceChange"
	// TriggerInputChange is a change of a Secret or ConfigMap holding input variables.
	TriggerInputChange RunTrigger = "InputChange"
	TriggerInterval    RunTrigger = "Interval"
	TriggerManual      RunTrigger = "Manual"
	// TriggerRetry replaces a Run whose plan became stale before it was applied.
	TriggerRetry    RunTrigger = "Retry"
	TriggerDeletion RunTrigger = "Deletion"
)

// FailureReason tells why a Run failed.
// +kubebuilder:validation:Enum=StepFailed;Timeout;StalePlan
type FailureReason string

const (
	// FailureStepFailed is a failure of the engine or of the runner.
	FailureStepFailed FailureReason = "StepFailed"
	// FailureTimeout is a step exceeding spec.runner.timeout.
	FailureTimeout FailureReason = "Timeout"
	// FailureStalePlan is a state changed between the plan and the apply.
	FailureStalePlan FailureReason = "StalePlan"
)

// RunPhase is the lifecycle step of a Run.
// +kubebuilder:validation:Enum=Pending;Planning;AwaitingApproval;Applying;Applied;NoChanges;Failed;Superseded
type RunPhase string

const (
	PhasePending          RunPhase = "Pending"
	PhasePlanning         RunPhase = "Planning"
	PhaseAwaitingApproval RunPhase = "AwaitingApproval"
	PhaseApplying         RunPhase = "Applying"
	PhaseApplied          RunPhase = "Applied"
	PhaseNoChanges        RunPhase = "NoChanges"
	PhaseFailed           RunPhase = "Failed"
	PhaseSuperseded       RunPhase = "Superseded"
)

// Labels set on the objects created for a Workspace or a Run.
const (
	WorkspaceLabel = "infra.labbs.fr/workspace"
	RunLabel       = "infra.labbs.fr/run"
	// RunNumberLabel orders the Runs of a Workspace.
	RunNumberLabel = "infra.labbs.fr/run-number"
)

// SupersedeAnnotation asks for a Run to be superseded, its value being the reason.
// It only takes effect while the Run has not started applying.
const SupersedeAnnotation = "infra.labbs.fr/supersede"

// PlanApproved tells whether the plan of the Run was approved.
func (r *Run) PlanApproved() bool {
	return r.Spec.ApprovedPlan != "" && r.Status.Plan != nil && r.Spec.ApprovedPlan == r.Status.Plan.Digest
}

// ApproveCommand is the kubectl command approving the plan of the Run.
func (r *Run) ApproveCommand() string {
	digest := ""
	if r.Status.Plan != nil {
		digest = r.Status.Plan.Digest
	}
	return fmt.Sprintf(`kubectl -n %s patch run %s --type merge -p '{"spec":{"approvedPlan":"%s"}}'`, r.Namespace, r.Name, digest)
}

// IsFinished tells whether the phase is terminal.
func (p RunPhase) IsFinished() bool {
	switch p {
	case PhaseApplied, PhaseNoChanges, PhaseFailed, PhaseSuperseded:
		return true
	}
	return false
}

// RunSpec defines the desired state of Run
type RunSpec struct {
	// workspaceRef is the Workspace this Run belongs to.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workspaceRef is immutable"
	// +required
	WorkspaceRef corev1.LocalObjectReference `json:"workspaceRef"`

	// operation applied once the plan is approved.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="operation is immutable"
	// +kubebuilder:default=Apply
	// +optional
	Operation RunOperation `json:"operation,omitempty"`

	// trigger is the reason the Run was created.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="trigger is immutable"
	// +optional
	Trigger RunTrigger `json:"trigger,omitempty"`

	// revision of the source used by the Run, resolved by the Workspace controller.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="revision is immutable"
	// +optional
	Revision string `json:"revision,omitempty"`

	// inputsRevision identifies the Secrets and ConfigMaps holding the input variables
	// when the Run was created.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="inputsRevision is immutable"
	// +optional
	InputsRevision string `json:"inputsRevision,omitempty"`

	// workspaceGeneration is the generation of the Workspace the Run was created for.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="workspaceGeneration is immutable"
	// +optional
	WorkspaceGeneration int64 `json:"workspaceGeneration,omitempty"`

	// autoApprove applies the plan without waiting for approval.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="autoApprove is immutable"
	// +optional
	AutoApprove bool `json:"autoApprove,omitempty"`

	// approvedPlan approves the plan whose digest (status.plan.digest) it holds. Binding the
	// approval to the digest guarantees that only the reviewed plan is applied.
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +optional
	ApprovedPlan string `json:"approvedPlan,omitempty"`
}

// PlanSummary is the result of a plan.
type PlanSummary struct {
	// hasChanges is true when resources or outputs change.
	// +optional
	HasChanges bool `json:"hasChanges,omitempty"`

	// add is the number of resources to create (replacements included).
	// +optional
	Add int32 `json:"add,omitempty"`

	// change is the number of resources to update in place.
	// +optional
	Change int32 `json:"change,omitempty"`

	// destroy is the number of resources to destroy (replacements included).
	// +optional
	Destroy int32 `json:"destroy,omitempty"`

	// import is the number of resources to import.
	// +optional
	Import int32 `json:"import,omitempty"`

	// digest is the sha256 of the plan file, so the applied plan can be checked
	// against the one that was reviewed.
	// +optional
	Digest string `json:"digest,omitempty"`
}

// RunStatus defines the observed state of Run.
type RunStatus struct {
	// observedGeneration is the last generation handled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// phase is the lifecycle step of the Run.
	// +optional
	Phase RunPhase `json:"phase,omitempty"`

	// conditions represent the current state of the Run resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// plan is the result of the plan step.
	// +optional
	Plan *PlanSummary `json:"plan,omitempty"`

	// planJob is the name of the Job running the plan step.
	// +optional
	PlanJob string `json:"planJob,omitempty"`

	// applyJob is the name of the Job running the apply step.
	// +optional
	ApplyJob string `json:"applyJob,omitempty"`

	// startedAt is the time the plan step started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// completedAt is the time the Run finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// message is a human readable detail about the phase, such as the error of a failed step.
	// +optional
	Message string `json:"message,omitempty"`

	// failureReason tells why the Run failed.
	// +optional
	FailureReason FailureReason `json:"failureReason,omitempty"`

	// notifiedEvents are the events already sent, so each one is sent once per Run.
	// +listType=set
	// +optional
	NotifiedEvents []NotificationEvent `json:"notifiedEvents,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=tofu
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.workspaceRef.name`
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.spec.operation`
// +kubebuilder:printcolumn:name="Trigger",type=string,JSONPath=`.spec.trigger`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Add",type=integer,JSONPath=`.status.plan.add`
// +kubebuilder:printcolumn:name="Change",type=integer,JSONPath=`.status.plan.change`
// +kubebuilder:printcolumn:name="Destroy",type=integer,JSONPath=`.status.plan.destroy`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.spec.revision`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Run is one execution (plan, then apply or destroy) of a Workspace.
type Run struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Run
	// +required
	Spec RunSpec `json:"spec"`

	// status defines the observed state of Run
	// +optional
	Status RunStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// RunList contains a list of Run
type RunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Run `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Run{}, &RunList{})
		return nil
	})
}
