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
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EngineType is the binary used to execute runs.
// +kubebuilder:validation:Enum=tofu;terraform
type EngineType string

const (
	EngineTofu      EngineType = "tofu"
	EngineTerraform EngineType = "terraform"
)

// EngineSpec selects the binary and the version used by the runner.
// +kubebuilder:validation:XValidation:rule="!has(self.type) || self.type != 'terraform' || has(self.version)",message="version is required when type is terraform"
type EngineSpec struct {
	// type is the binary used to execute runs.
	// +kubebuilder:default=tofu
	// +optional
	Type EngineType `json:"type,omitempty"`

	// version is the exact version to use, e.g. "1.10.0".
	// When empty, the version bundled in the runner image is used (tofu only).
	// +kubebuilder:validation:Pattern=`^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`
	// +optional
	Version string `json:"version,omitempty"`
}

// SourceSpec describes where the configuration comes from. Exactly one field must be set.
// +kubebuilder:validation:XValidation:rule="(has(self.git) ? 1 : 0) + (has(self.oci) ? 1 : 0) + (has(self.configMap) ? 1 : 0) + (has(self.inline) ? 1 : 0) == 1",message="exactly one of git, oci, configMap or inline must be set"
type SourceSpec struct {
	// git fetches the configuration from a Git repository.
	// +optional
	Git *GitSource `json:"git,omitempty"`

	// oci fetches the configuration from an OCI artifact stored in a container registry.
	// +optional
	OCI *OCISource `json:"oci,omitempty"`

	// configMap loads the configuration from a ConfigMap, each key being written as a file.
	// +optional
	ConfigMap *ConfigMapSource `json:"configMap,omitempty"`

	// inline is the content of a single main.tf file. It is passed to the runner in an
	// environment variable, hence its size limit; use a ConfigMap or git for larger configurations.
	// +kubebuilder:validation:MaxLength=65536
	// +optional
	Inline string `json:"inline,omitempty"`
}

// GitSource is a Git repository holding the root module.
type GitSource struct {
	// url of the repository (https://, ssh:// or git@host:org/repo.git).
	// +kubebuilder:validation:MinLength=1
	// +required
	URL string `json:"url"`

	// ref to check out. Defaults to the remote HEAD.
	// +optional
	Ref GitRef `json:"ref,omitzero"`

	// path of the root module, relative to the repository root.
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && !self.contains('..')",message="path must be relative and must not contain '..'"
	// +optional
	Path string `json:"path,omitempty"`

	// secretRef references a Secret holding the credentials.
	// HTTPS: keys `username` and `password` (a token can be used as password).
	// SSH: keys `identity` (private key) and `known_hosts`.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// interval at which the remote is polled for new commits.
	// +kubebuilder:default="1m"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
}

// GitRef selects what to check out. At most one field can be set.
// +kubebuilder:validation:XValidation:rule="(has(self.branch) ? 1 : 0) + (has(self.tag) ? 1 : 0) + (has(self.commit) ? 1 : 0) <= 1",message="at most one of branch, tag or commit can be set"
type GitRef struct {
	// branch to track.
	// +optional
	Branch string `json:"branch,omitempty"`

	// tag to check out.
	// +optional
	Tag string `json:"tag,omitempty"`

	// commit SHA to check out.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	// +optional
	Commit string `json:"commit,omitempty"`
}

// OCISource is an OCI artifact holding the root module, pushed for instance with
// `oras push` (one layer per file) or `flux push artifact` (a tar.gz layer).
type OCISource struct {
	// url of the repository, e.g. oci://ghcr.io/acme/modules/vpc.
	// +kubebuilder:validation:Pattern=`^oci://[^/]+/.+$`
	// +required
	URL string `json:"url"`

	// ref to pull. Defaults to the latest tag.
	// +optional
	Ref OCIRef `json:"ref,omitzero"`

	// path of the root module inside the artifact.
	// +kubebuilder:validation:XValidation:rule="!self.startsWith('/') && !self.contains('..')",message="path must be relative and must not contain '..'"
	// +optional
	Path string `json:"path,omitempty"`

	// secretRef references a Secret of type kubernetes.io/dockerconfigjson with the
	// credentials of the registry.
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`

	// insecure allows plain HTTP registries.
	// +optional
	Insecure bool `json:"insecure,omitempty"`

	// interval at which the tag is checked for a new digest.
	// +kubebuilder:default="1m"
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`
}

// OCIRef selects the artifact to pull. At most one field can be set.
// +kubebuilder:validation:XValidation:rule="!(has(self.tag) && has(self.digest))",message="at most one of tag or digest can be set"
type OCIRef struct {
	// tag to pull.
	// +optional
	Tag string `json:"tag,omitempty"`

	// digest to pull, e.g. sha256:4f2c...
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +optional
	Digest string `json:"digest,omitempty"`
}

// ConfigMapSource loads the configuration from a ConfigMap.
type ConfigMapSource struct {
	// name of the ConfigMap in the namespace of the Workspace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`
}

// Variable is an input variable of the root module. Exactly one of value or valueFrom
// must be set; this is checked by the controller because CEL cannot see untyped fields.
type Variable struct {
	// name of the variable.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_-]*$`
	// +required
	Name string `json:"name"`

	// value of the variable. Strings are passed as is; numbers, booleans, lists and
	// objects are encoded as JSON and parsed according to the variable's declared type.
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`

	// valueFrom reads the value from a key of a Secret or a ConfigMap.
	// +optional
	ValueFrom *VariableSource `json:"valueFrom,omitempty"`
}

// VariableSource selects a key of a Secret or a ConfigMap.
// +kubebuilder:validation:XValidation:rule="has(self.secretKeyRef) != has(self.configMapKeyRef)",message="exactly one of secretKeyRef or configMapKeyRef must be set"
type VariableSource struct {
	// secretKeyRef selects a key of a Secret.
	// +optional
	SecretKeyRef *corev1.SecretKeySelector `json:"secretKeyRef,omitempty"`

	// configMapKeyRef selects a key of a ConfigMap.
	// +optional
	ConfigMapKeyRef *corev1.ConfigMapKeySelector `json:"configMapKeyRef,omitempty"`
}

// VarsFromSource imports every key of a Secret or a ConfigMap as an input variable.
type VarsFromSource struct {
	// kind of the referenced object.
	// +kubebuilder:validation:Enum=Secret;ConfigMap
	// +required
	Kind string `json:"kind"`

	// name of the referenced object, in the namespace of the Workspace.
	// +kubebuilder:validation:MinLength=1
	// +required
	Name string `json:"name"`

	// optional makes the run proceed when the object does not exist.
	// +optional
	Optional bool `json:"optional,omitempty"`
}

// BackendType selects where the state is stored.
// +kubebuilder:validation:Enum=Kubernetes;Source;Custom
type BackendType string

const (
	// BackendKubernetes stores the state in a Secret of the Workspace namespace.
	BackendKubernetes BackendType = "Kubernetes"
	// BackendSource uses the backend declared in the configuration itself.
	BackendSource BackendType = "Source"
	// BackendCustom uses the backend block given in the Workspace.
	BackendCustom BackendType = "Custom"
)

// BackendSpec configures the state backend.
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'Custom') == has(self.custom)",message="custom must be set if and only if type is Custom"
type BackendSpec struct {
	// type of state backend.
	// Kubernetes stores the state in a Secret of the Workspace namespace.
	// Source uses the backend declared in the configuration itself.
	// Custom uses the backend block given in `custom`.
	// +kubebuilder:default=Kubernetes
	// +optional
	Type BackendType `json:"type,omitempty"`

	// custom is a complete `terraform { backend "..." { ... } }` block, written as an override file.
	// Credentials should be passed through env or envFrom.
	// +kubebuilder:validation:MaxLength=16384
	// +optional
	Custom string `json:"custom,omitempty"`
}

// PlanStorageType selects where plans are kept between the plan and apply steps.
// +kubebuilder:validation:Enum=Secret;S3;Postgres
type PlanStorageType string

const (
	// PlanStorageSecret keeps plans in Secrets, limited to 1 MiB.
	PlanStorageSecret PlanStorageType = "Secret"
	// PlanStorageS3 keeps plans in a bucket of S3 or of a compatible service (MinIO, R2...).
	PlanStorageS3 PlanStorageType = "S3"
	// PlanStoragePostgres keeps plans in a PostgreSQL table.
	PlanStoragePostgres PlanStorageType = "Postgres"
)

// PlanStorageSpec configures where plans are kept between the plan and apply steps.
// Plans hold a snapshot of the state and the values of the variables: the storage must
// be as protected as the state.
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'S3') == has(self.s3)",message="s3 must be set if and only if type is S3"
// +kubebuilder:validation:XValidation:rule="(has(self.type) && self.type == 'Postgres') == has(self.postgres)",message="postgres must be set if and only if type is Postgres"
type PlanStorageSpec struct {
	// type of storage.
	// +kubebuilder:default=Secret
	// +optional
	Type PlanStorageType `json:"type,omitempty"`

	// s3 configures the S3 storage.
	// +optional
	S3 *S3PlanStorage `json:"s3,omitempty"`

	// postgres configures the PostgreSQL storage.
	// +optional
	Postgres *PostgresPlanStorage `json:"postgres,omitempty"`
}

// S3PlanStorage is a bucket of S3 or of a compatible service.
type S3PlanStorage struct {
	// bucket receiving the plans.
	// +kubebuilder:validation:MinLength=3
	// +required
	Bucket string `json:"bucket"`

	// prefix of the object keys.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// region of the bucket.
	// +optional
	Region string `json:"region,omitempty"`

	// endpoint of an S3 compatible service, e.g. https://minio.example.com.
	// Defaults to AWS S3.
	// +kubebuilder:validation:Pattern=`^https?://`
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// pathStyle uses path-style URLs (endpoint/bucket/key), required by some compatible services.
	// +optional
	PathStyle bool `json:"pathStyle,omitempty"`

	// secretRef references a Secret with the keys AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
	// and optionally AWS_SESSION_TOKEN. Without it, the credentials come from the
	// environment of the runner (spec.env) or from its ServiceAccount (IRSA, pod identity).
	// +optional
	SecretRef *corev1.LocalObjectReference `json:"secretRef,omitempty"`
}

// PostgresPlanStorage is a PostgreSQL database.
type PostgresPlanStorage struct {
	// secretRef references a Secret with the connection URL in the `url` key,
	// e.g. postgres://user:password@host:5432/db?sslmode=require.
	// +required
	SecretRef corev1.LocalObjectReference `json:"secretRef"`

	// table receiving the plans; it is created if needed.
	// +kubebuilder:validation:Pattern=`^[a-z_][a-z0-9_]*$`
	// +kubebuilder:default=tofu_plans
	// +optional
	Table string `json:"table,omitempty"`
}

// ApprovalMode tells whether plans are applied automatically.
// +kubebuilder:validation:Enum=Auto;Manual
type ApprovalMode string

const (
	ApprovalAuto   ApprovalMode = "Auto"
	ApprovalManual ApprovalMode = "Manual"
)

// OutputsSpec exports the outputs of the root module.
type OutputsSpec struct {
	// secretName is the Secret receiving one key per output.
	// String outputs are written as is, other types as JSON.
	// +kubebuilder:validation:MinLength=1
	// +required
	SecretName string `json:"secretName"`
}

// RunnerSpec customizes the pods executing the runs.
type RunnerSpec struct {
	// image overrides the runner image.
	// +optional
	Image string `json:"image,omitempty"`

	// serviceAccountName used by the runner pods. When empty, the operator manages a
	// ServiceAccount with the permissions required by the Kubernetes backend.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// resources of the runner container.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitzero"`

	// nodeSelector of the runner pods.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations of the runner pods.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// podLabels are added to the runner pods (e.g. for workload identity).
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// podAnnotations are added to the runner pods.
	// +optional
	PodAnnotations map[string]string `json:"podAnnotations,omitempty"`

	// timeout of each step (plan, apply).
	// +kubebuilder:default="1h"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// NotificationSpec sends events of the Workspace to a NotificationProvider.
type NotificationSpec struct {
	// providerRef is a NotificationProvider in the namespace of the Workspace.
	// +required
	ProviderRef corev1.LocalObjectReference `json:"providerRef"`

	// events to send. All events are sent when empty.
	// +optional
	Events []NotificationEvent `json:"events,omitempty"`
}

// WorkspaceSpec defines the desired state of Workspace
type WorkspaceSpec struct {
	// engine selects the binary and its version.
	// +kubebuilder:default={}
	// +optional
	Engine EngineSpec `json:"engine,omitzero"`

	// source of the configuration.
	// +required
	Source SourceSpec `json:"source"`

	// vars are input variables of the root module. They take precedence over varsFrom.
	// +listType=map
	// +listMapKey=name
	// +optional
	Vars []Variable `json:"vars,omitempty"`

	// varsFrom imports input variables from Secrets or ConfigMaps.
	// When a key is present in several sources, the last one wins.
	// +optional
	VarsFrom []VarsFromSource `json:"varsFrom,omitempty"`

	// env sets environment variables of the runner (provider credentials, TF_LOG...).
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// envFrom sets environment variables of the runner from Secrets or ConfigMaps.
	// +optional
	EnvFrom []corev1.EnvFromSource `json:"envFrom,omitempty"`

	// backend configures where the state is stored.
	// +kubebuilder:default={}
	// +optional
	Backend BackendSpec `json:"backend,omitzero"`

	// planStorage configures where plans are kept between the plan and apply steps.
	// +kubebuilder:default={}
	// +optional
	PlanStorage PlanStorageSpec `json:"planStorage,omitzero"`

	// approvalMode tells whether plans with changes are applied automatically
	// or wait for the Run to be approved.
	// +kubebuilder:default=Auto
	// +optional
	ApprovalMode ApprovalMode `json:"approvalMode,omitempty"`

	// interval at which the Workspace is planned again to detect drift.
	// Drift detection is disabled when unset.
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// suspend prevents new runs from being created.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// destroyOnDelete destroys the managed resources when the Workspace is deleted.
	// +optional
	DestroyOnDelete bool `json:"destroyOnDelete,omitempty"`

	// outputs exports the outputs of the root module to a Secret.
	// +optional
	Outputs *OutputsSpec `json:"outputs,omitempty"`

	// runner customizes the pods executing the runs.
	// +kubebuilder:default={}
	// +optional
	Runner RunnerSpec `json:"runner,omitzero"`

	// notifications sends events to NotificationProviders.
	// +optional
	Notifications []NotificationSpec `json:"notifications,omitempty"`

	// runHistoryLimit is the number of finished Runs kept for this Workspace.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=1
	// +optional
	RunHistoryLimit *int32 `json:"runHistoryLimit,omitempty"`
}

// Condition types and reasons of a Workspace.
const (
	// ConditionReady is True when the last run for the current generation and revision succeeded.
	ConditionReady = "Ready"
	// ConditionSourceReady is True when the source revision could be resolved.
	ConditionSourceReady = "SourceReady"

	ReasonSucceeded        = "Succeeded"
	ReasonProgressing      = "Progressing"
	ReasonApprovalRequired = "ApprovalRequired"
	ReasonRunFailed        = "RunFailed"
	ReasonSourceFailed     = "SourceFailed"
	ReasonSuspended        = "Suspended"
	ReasonDeleting         = "Deleting"
	ReasonDestroyFailed    = "DestroyFailed"
	ReasonInvalidSpec      = "InvalidSpec"
	ReasonInputsFailed     = "InputsFailed"
)

// RunReference summarizes a Run of the Workspace.
type RunReference struct {
	// name of the Run.
	// +required
	Name string `json:"name"`

	// phase of the Run.
	// +optional
	Phase RunPhase `json:"phase,omitempty"`

	// completedAt is the time the Run finished.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
}

// WorkspaceStatus defines the observed state of Workspace.
type WorkspaceStatus struct {
	// observedGeneration is the last generation handled by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// conditions represent the current state of the Workspace resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// sourceRevision is the latest revision resolved from the source.
	// +optional
	SourceRevision string `json:"sourceRevision,omitempty"`

	// inputsRevision identifies the current versions of the Secrets and ConfigMaps
	// holding the input variables. A change triggers a new run.
	// +optional
	InputsRevision string `json:"inputsRevision,omitempty"`

	// lastAppliedRevision is the source revision of the last successful apply.
	// +optional
	LastAppliedRevision string `json:"lastAppliedRevision,omitempty"`

	// lastAppliedAt is the time of the last successful apply.
	// +optional
	LastAppliedAt *metav1.Time `json:"lastAppliedAt,omitempty"`

	// currentRun is the Run in progress, if any.
	// +optional
	CurrentRun string `json:"currentRun,omitempty"`

	// lastRun is the most recent finished Run.
	// +optional
	LastRun *RunReference `json:"lastRun,omitempty"`

	// lastHandledRequestedAt is the last value of the requested-at annotation that triggered a run.
	// +optional
	LastHandledRequestedAt string `json:"lastHandledRequestedAt,omitempty"`
}

// RequestedAtAnnotation triggers a new run when its value changes.
const RequestedAtAnnotation = "infra.labbs.fr/requested-at"

// RefreshRequestedAtAnnotation makes the controller check the source right away, without
// forcing a run. It is set by the Receivers.
const RefreshRequestedAtAnnotation = "infra.labbs.fr/refresh-requested-at"

// Finalizer lets the controller destroy the managed resources before a Workspace is deleted.
const Finalizer = "infra.labbs.fr/finalizer"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=ws,categories=tofu
// +kubebuilder:validation:XValidation:rule="self.metadata.name.size() <= 63",message="name must be at most 63 characters, as it is used in label values"
// +kubebuilder:printcolumn:name="Engine",type=string,JSONPath=`.spec.engine.type`
// +kubebuilder:printcolumn:name="Approval",type=string,JSONPath=`.spec.approvalMode`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].reason`
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=`.status.currentRun`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.status.lastAppliedRevision`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workspace is the Schema for the workspaces API
type Workspace struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Workspace
	// +required
	Spec WorkspaceSpec `json:"spec"`

	// status defines the observed state of Workspace
	// +optional
	Status WorkspaceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// WorkspaceList contains a list of Workspace
type WorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Workspace `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Workspace{}, &WorkspaceList{})
		return nil
	})
}
