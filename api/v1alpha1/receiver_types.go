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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ReceiverType is the sender of the webhook calls, which defines how they are authenticated.
// +kubebuilder:validation:Enum=github;gitlab;generic
type ReceiverType string

const (
	// ReceiverGitHub checks the X-Hub-Signature-256 HMAC and reacts to push events.
	ReceiverGitHub ReceiverType = "github"
	// ReceiverGitLab checks the X-Gitlab-Token header and reacts to push and tag push events.
	ReceiverGitLab ReceiverType = "gitlab"
	// ReceiverGeneric checks the `Authorization: Bearer <token>` header, e.g. for a CI job
	// calling the receiver after pushing an OCI artifact.
	ReceiverGeneric ReceiverType = "generic"
)

// ReceiverTokenKey is the key of the token in the Secret of a Receiver.
const ReceiverTokenKey = "token"

// ReceiverSpec defines the desired state of Receiver
type ReceiverSpec struct {
	// type of the sender of the webhook calls.
	// +required
	Type ReceiverType `json:"type"`

	// secretRef references a Secret holding the shared token in the `token` key.
	// The token authenticates the calls and is part of the webhook path.
	// +required
	SecretRef corev1.LocalObjectReference `json:"secretRef"`

	// workspaceSelector selects the Workspaces of the namespace whose source is checked
	// again on each call. All the Workspaces of the namespace are selected when unset.
	// +optional
	WorkspaceSelector *metav1.LabelSelector `json:"workspaceSelector,omitempty"`
}

// ReceiverStatus defines the observed state of Receiver.
type ReceiverStatus struct {
	// observedGeneration is the last generation handled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the Receiver resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// webhookPath is the path to call on the receiver endpoint of the operator.
	// It is derived from the token, so it changes when the token does.
	// +optional
	WebhookPath string `json:"webhookPath,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=tofu
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Path",type=string,JSONPath=`.status.webhookPath`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Receiver is a webhook endpoint that makes Workspaces check their source right away,
// for instance after a git push, instead of waiting for the next poll.
type Receiver struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Receiver
	// +required
	Spec ReceiverSpec `json:"spec"`

	// status defines the observed state of Receiver
	// +optional
	Status ReceiverStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ReceiverList contains a list of Receiver
type ReceiverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Receiver `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Receiver{}, &ReceiverList{})
		return nil
	})
}
