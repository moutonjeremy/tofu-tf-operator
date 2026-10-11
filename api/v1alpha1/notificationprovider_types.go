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

// NotificationEvent is an event of a Run that can be notified.
// +kubebuilder:validation:Enum=PlanHasChanges;PlanFailed;ApprovalRequired;ApplySucceeded;ApplyFailed;DriftDetected;DestroySucceeded;DestroyFailed
type NotificationEvent string

const (
	EventPlanHasChanges   NotificationEvent = "PlanHasChanges"
	EventPlanFailed       NotificationEvent = "PlanFailed"
	EventApprovalRequired NotificationEvent = "ApprovalRequired"
	EventApplySucceeded   NotificationEvent = "ApplySucceeded"
	EventApplyFailed      NotificationEvent = "ApplyFailed"
	EventDriftDetected    NotificationEvent = "DriftDetected"
	EventDestroySucceeded NotificationEvent = "DestroySucceeded"
	EventDestroyFailed    NotificationEvent = "DestroyFailed"
)

// ProviderType is the kind of endpoint events are sent to.
// +kubebuilder:validation:Enum=webhook
type ProviderType string

const (
	ProviderWebhook ProviderType = "webhook"
)

// NotificationProviderSpec defines the desired state of NotificationProvider
// +kubebuilder:validation:XValidation:rule="has(self.address) || has(self.secretRef)",message="one of address or secretRef must be set"
type NotificationProviderSpec struct {
	// type of the provider.
	// webhook posts the event as JSON to the address.
	// +kubebuilder:default=webhook
	// +optional
	Type ProviderType `json:"type,omitempty"`

	// address is the URL events are sent to.
	// Use secretRef instead when the URL contains a token.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	Address string `json:"address,omitempty"`

	// secretRef references a Secret with optional keys:
	// `address` (overrides address) and `token` (sent as `Authorization: Bearer <token>`).
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// timeout of each request.
	// +kubebuilder:default="15s"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`

	// suspend stops sending events.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=tofu
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// NotificationProvider is an endpoint receiving the events of Workspaces.
type NotificationProvider struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of NotificationProvider
	// +required
	Spec NotificationProviderSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// NotificationProviderList contains a list of NotificationProvider
type NotificationProviderList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NotificationProvider `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NotificationProvider{}, &NotificationProviderList{})
		return nil
	})
}
