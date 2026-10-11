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
	"context"
	"fmt"
	"maps"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/metrics"
	"github.com/labbs/tofu-tf-operator/internal/source"
)

const (
	// referencedObjectsIndex indexes Workspaces by the Secrets and ConfigMaps ("Kind/name")
	// whose changes trigger a run: the ConfigMap source and the input variables.
	referencedObjectsIndex = ".spec.referencedObjects"
	// sourceRetryInterval is the delay before resolving a source again after an error.
	sourceRetryInterval = time.Minute
	defaultHistoryLimit = 10
)

// GitResolver resolves the revision of a git ref.
type GitResolver func(ctx context.Context, url string, ref infrav1alpha1.GitRef, secret map[string][]byte) (string, error)

// WorkspaceReconciler decides when a Workspace needs a Run.
type WorkspaceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
	// ResolveGit defaults to source.ResolveGit.
	ResolveGit GitResolver
	// Now defaults to time.Now.
	Now func() time.Time
}

// +kubebuilder:rbac:groups=infra.labbs.fr,resources=workspaces,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=workspaces/finalizers,verbs=update
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=runs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch
// The runner Role grants these permissions, so the operator must hold them too.
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;create;update;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile resolves the source of a Workspace, creates its Runs and reports their outcome.
func (r *WorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ws := &infrav1alpha1.Workspace{}
	if err := r.Get(ctx, req.NamespacedName, ws); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	runs, err := r.listRuns(ctx, ws)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ws.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, ws, runs)
	}
	// The update does not change the generation, so it would not trigger a new
	// reconciliation: carry on with the updated object.
	if controllerutil.AddFinalizer(ws, infrav1alpha1.Finalizer) {
		if err := r.Update(ctx, ws); err != nil {
			return ctrl.Result{}, err
		}
	}

	original := ws.DeepCopy()
	result, err := r.reconcileRuns(ctx, ws, runs)
	ws.Status.ObservedGeneration = ws.Generation
	if patchErr := r.Status().Patch(ctx, ws, client.MergeFrom(original)); patchErr != nil {
		return ctrl.Result{}, client.IgnoreNotFound(patchErr)
	}
	if ready := meta.FindStatusCondition(ws.Status.Conditions, infrav1alpha1.ConditionReady); ready != nil {
		metrics.SetWorkspaceReady(ws.Namespace, ws.Name, ready.Reason, ready.Status == metav1.ConditionTrue)
	}
	return result, err
}

// reconcileRuns updates the status of the Workspace and creates the next Run when needed.
func (r *WorkspaceReconciler) reconcileRuns(ctx context.Context, ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run) (ctrl.Result, error) {
	if err := validateWorkspace(ws); err != nil {
		setCondition(ws, infrav1alpha1.ConditionReady, metav1.ConditionFalse, infrav1alpha1.ReasonInvalidSpec, err.Error())
		return ctrl.Result{}, nil
	}

	revision, pollInterval, err := r.resolveRevision(ctx, ws)
	if err != nil {
		setCondition(ws, infrav1alpha1.ConditionSourceReady, metav1.ConditionFalse, infrav1alpha1.ReasonSourceFailed, err.Error())
		computeStatus(ws, runs, ws.Status.SourceRevision)
		return ctrl.Result{RequeueAfter: sourceRetryInterval}, nil
	}
	ws.Status.SourceRevision = revision
	setCondition(ws, infrav1alpha1.ConditionSourceReady, metav1.ConditionTrue, infrav1alpha1.ReasonSucceeded, "revision "+revision)

	inputs, err := r.resolveInputs(ctx, ws)
	if err != nil {
		// The runner pod could not start without a required Secret or ConfigMap.
		setCondition(ws, infrav1alpha1.ConditionReady, metav1.ConditionFalse, infrav1alpha1.ReasonInputsFailed, err.Error())
		return ctrl.Result{RequeueAfter: sourceRetryInterval}, nil
	}
	ws.Status.InputsRevision = inputs

	if !ws.Spec.Suspend {
		created, err := r.triggerRun(ctx, ws, runs, revision, inputs)
		if err != nil {
			return ctrl.Result{}, err
		}
		if created != nil {
			runs = append(runs, *created)
		}
	}
	computeStatus(ws, runs, revision)
	if err := r.deleteOldRuns(ctx, ws, runs); err != nil {
		return ctrl.Result{}, err
	}

	// Come back for the next poll of the source or the next drift detection.
	requeue := pollInterval
	if due, ok := nextIntervalRun(ws, lastRun(runs, notSuperseded)); ok && !ws.Spec.Suspend {
		wait := max(due.Sub(r.now()), time.Second)
		if requeue == 0 || wait < requeue {
			requeue = wait
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// triggerRun creates the next Run, or asks the active one to be superseded first.
func (r *WorkspaceReconciler) triggerRun(ctx context.Context, ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run,
	revision, inputs string) (*infrav1alpha1.Run, error) {
	trigger := nextTrigger(ws, runs, revision, inputs, r.now())
	if trigger == "" {
		return nil, nil
	}
	if active := activeRun(runs); active != nil {
		// Drift detection waits; other triggers replace a Run that has not started applying.
		if trigger != infrav1alpha1.TriggerInterval && cancellable(active) {
			return nil, r.supersede(ctx, ws, active, string(trigger))
		}
		return nil, nil
	}
	run, err := r.createRun(ctx, ws, runs, trigger, revision, inputs)
	if err == nil && trigger == infrav1alpha1.TriggerManual {
		ws.Status.LastHandledRequestedAt = ws.Annotations[infrav1alpha1.RequestedAtAnnotation]
	}
	return run, err
}

func (r *WorkspaceReconciler) createRun(ctx context.Context, ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run,
	trigger infrav1alpha1.RunTrigger, revision, inputs string) (*infrav1alpha1.Run, error) {
	if ws.Spec.Runner.ServiceAccountName == "" {
		if err := r.ensureRunnerRBAC(ctx, ws.Namespace); err != nil {
			return nil, fmt.Errorf("creating the runner ServiceAccount: %w", err)
		}
	}
	run := newRun(ws, runs, trigger, revision, inputs)
	if err := controllerutil.SetControllerReference(ws, run, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, run); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Created by a previous reconciliation that the cache does not show yet.
			return nil, nil
		}
		return nil, err
	}
	logf.FromContext(ctx).Info("run created", "run", run.Name, "trigger", trigger, "revision", revision)
	r.event(ws, run, corev1.EventTypeNormal, "RunCreated", "CreateRun", "Created run %s (%s, revision %s)", run.Name, trigger, revision)
	return run, nil
}

// supersede asks the Run controller to stop a Run that has not started applying.
func (r *WorkspaceReconciler) supersede(ctx context.Context, ws *infrav1alpha1.Workspace, run *infrav1alpha1.Run, reason string) error {
	if _, ok := run.Annotations[infrav1alpha1.SupersedeAnnotation]; ok {
		return nil
	}
	patch := client.MergeFrom(run.DeepCopy())
	if run.Annotations == nil {
		run.Annotations = map[string]string{}
	}
	run.Annotations[infrav1alpha1.SupersedeAnnotation] = reason
	if err := r.Patch(ctx, run, patch); err != nil {
		return client.IgnoreNotFound(err)
	}
	r.event(ws, run, corev1.EventTypeNormal, "SupersedeRequested", "Supersede", "Run %s superseded (%s)", run.Name, reason)
	return nil
}

// reconcileDelete destroys the managed resources if requested, then releases the Workspace.
func (r *WorkspaceReconciler) reconcileDelete(ctx context.Context, ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(ws, infrav1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	release := func() (ctrl.Result, error) {
		controllerutil.RemoveFinalizer(ws, infrav1alpha1.Finalizer)
		if err := r.Update(ctx, ws); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		metrics.ForgetWorkspace(ws.Namespace, ws.Name)
		return ctrl.Result{}, nil
	}
	// Nothing to destroy if no apply ever started.
	applyStarted := lastRun(runs, func(run *infrav1alpha1.Run) bool { return run.Status.ApplyJob != "" }) != nil
	if !ws.Spec.DestroyOnDelete || !applyStarted {
		return release()
	}

	original := ws.DeepCopy()
	setStatus := func(reason, message string) (ctrl.Result, error) {
		setCondition(ws, infrav1alpha1.ConditionReady, metav1.ConditionFalse, reason, message)
		return ctrl.Result{}, client.IgnoreNotFound(r.Status().Patch(ctx, ws, client.MergeFrom(original)))
	}

	isDestroy := func(run *infrav1alpha1.Run) bool { return run.Spec.Trigger == infrav1alpha1.TriggerDeletion }
	destroy := lastRun(runs, isDestroy)
	if active := activeRun(runs); active != nil && !isDestroy(active) {
		if cancellable(active) {
			if err := r.supersede(ctx, ws, active, "Deletion"); err != nil {
				return ctrl.Result{}, err
			}
		}
		return setStatus(infrav1alpha1.ReasonDeleting, fmt.Sprintf("waiting for run %s before destroying", active.Name))
	}

	requested := ws.Annotations[infrav1alpha1.RequestedAtAnnotation]
	retry := destroy != nil && destroy.Status.Phase == infrav1alpha1.PhaseFailed &&
		requested != "" && requested != ws.Status.LastHandledRequestedAt
	if destroy == nil || retry {
		run, err := r.createRun(ctx, ws, runs, infrav1alpha1.TriggerDeletion, ws.Status.SourceRevision, ws.Status.InputsRevision)
		if err != nil {
			return ctrl.Result{}, err
		}
		if retry {
			ws.Status.LastHandledRequestedAt = requested
		}
		name := ""
		if run != nil {
			name = run.Name
		}
		return setStatus(infrav1alpha1.ReasonDeleting, "destroying with run "+name)
	}

	switch destroy.Status.Phase {
	case infrav1alpha1.PhaseApplied, infrav1alpha1.PhaseNoChanges:
		return release()
	case infrav1alpha1.PhaseFailed:
		return setStatus(infrav1alpha1.ReasonDestroyFailed, fmt.Sprintf(
			"run %s failed: %s; set the %s annotation to retry, or destroyOnDelete to false to give up",
			destroy.Name, firstLine(destroy.Status.Message), infrav1alpha1.RequestedAtAnnotation))
	}
	return setStatus(infrav1alpha1.ReasonDeleting, "destroying with run "+destroy.Name)
}

// resolveRevision returns the current revision of the source and how often to poll it.
func (r *WorkspaceReconciler) resolveRevision(ctx context.Context, ws *infrav1alpha1.Workspace) (string, time.Duration, error) {
	src := ws.Spec.Source
	switch {
	case src.Git != nil:
		var secret map[string][]byte
		if src.Git.SecretRef != nil {
			s := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: src.Git.SecretRef.Name}, s); err != nil {
				return "", 0, fmt.Errorf("reading the git Secret: %w", err)
			}
			secret = s.Data
		}
		resolve := r.ResolveGit
		if resolve == nil {
			resolve = source.ResolveGit
		}
		ref := src.Git.Ref.Branch + src.Git.Ref.Tag + src.Git.Ref.Commit
		revision, err := resolve(ctx, src.Git.URL, src.Git.Ref, secret)
		if err != nil {
			return "", 0, fmt.Errorf("resolving %s: %w", cmp.Or(ref, "HEAD"), err)
		}
		interval := time.Minute
		if src.Git.Interval != nil && src.Git.Interval.Duration > 0 {
			interval = src.Git.Interval.Duration
		}
		return revision, interval, nil

	case src.OCI != nil:
		opts := source.OCIOptions{URL: src.OCI.URL, Insecure: src.OCI.Insecure}
		if src.OCI.SecretRef != nil {
			s := &corev1.Secret{}
			if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: src.OCI.SecretRef.Name}, s); err != nil {
				return "", 0, fmt.Errorf("reading the registry Secret: %w", err)
			}
			opts.DockerConfig = s.Data[source.DockerConfigKey]
		}
		revision, err := source.ResolveOCI(ctx, opts, src.OCI.Ref.Tag, src.OCI.Ref.Digest)
		if err != nil {
			return "", 0, err
		}
		interval := time.Minute
		if src.OCI.Interval != nil && src.OCI.Interval.Duration > 0 {
			interval = src.OCI.Interval.Duration
		}
		return revision, interval, nil

	case src.ConfigMap != nil:
		cm := &corev1.ConfigMap{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: src.ConfigMap.Name}, cm); err != nil {
			return "", 0, fmt.Errorf("reading the ConfigMap: %w", err)
		}
		files := make(map[string][]byte, len(cm.Data)+len(cm.BinaryData))
		for name, content := range cm.Data {
			files[name] = []byte(content)
		}
		maps.Copy(files, cm.BinaryData)
		return source.FilesRevision(files), 0, nil
	}
	return source.InlineRevision(src.Inline), 0, nil
}

// resolveInputs returns the revision of the Secrets and ConfigMaps holding the input
// variables, from their metadata only. It also checks that the Secret of the plan
// storage exists, as the runner pods could not start without it.
func (r *WorkspaceReconciler) resolveInputs(ctx context.Context, ws *infrav1alpha1.Workspace) (string, error) {
	if name := planStorageSecret(ws); name != "" {
		obj := &metav1.PartialObjectMetadata{}
		obj.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		if err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: name}, obj); err != nil {
			return "", fmt.Errorf("reading the Secret %s of the plan storage: %w", name, err)
		}
	}
	var versions []string
	for _, ref := range inputRefs(ws) {
		obj := &metav1.PartialObjectMetadata{}
		obj.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind(ref.kind))
		err := r.Get(ctx, types.NamespacedName{Namespace: ws.Namespace, Name: ref.name}, obj)
		switch {
		case apierrors.IsNotFound(err) && ref.optional:
			versions = append(versions, ref.String()+"=absent")
		case err != nil:
			return "", fmt.Errorf("reading %s %s of the input variables: %w", ref.kind, ref.name, err)
		default:
			versions = append(versions, ref.String()+"="+obj.ResourceVersion)
		}
	}
	return inputsRevision(versions), nil
}

// ensureRunnerRBAC creates the ServiceAccount used by the runner pods and its permissions:
// Secrets and Leases for the Kubernetes backend, Secrets for the plans and outputs.
func (r *WorkspaceReconciler) ensureRunnerRBAC(ctx context.Context, namespace string) error {
	labels := map[string]string{"app.kubernetes.io/managed-by": "tofu-tf-operator"}
	objectMeta := metav1.ObjectMeta{Name: DefaultRunnerServiceAccount, Namespace: namespace}

	sa := &corev1.ServiceAccount{ObjectMeta: objectMeta}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, sa, func() error {
		sa.Labels = labels
		return nil
	}); err != nil {
		return err
	}
	role := &rbacv1.Role{ObjectMeta: objectMeta}
	if _, err := controllerutil.CreateOrPatch(ctx, r.Client, role, func() error {
		role.Labels = labels
		role.Rules = []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"}},
		}
		return nil
	}); err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: objectMeta}
	_, err := controllerutil.CreateOrPatch(ctx, r.Client, binding, func() error {
		binding.Labels = labels
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: DefaultRunnerServiceAccount}
		binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: DefaultRunnerServiceAccount, Namespace: namespace}}
		return nil
	})
	return err
}

// listRuns returns the Runs of a Workspace, oldest first.
func (r *WorkspaceReconciler) listRuns(ctx context.Context, ws *infrav1alpha1.Workspace) ([]infrav1alpha1.Run, error) {
	list := &infrav1alpha1.RunList{}
	if err := r.List(ctx, list, client.InNamespace(ws.Namespace), client.MatchingLabels{infrav1alpha1.WorkspaceLabel: ws.Name}); err != nil {
		return nil, err
	}
	sortRuns(list.Items)
	return list.Items, nil
}

// deleteOldRuns enforces the history limit. The Jobs and Secrets of a Run are garbage collected with it.
func (r *WorkspaceReconciler) deleteOldRuns(ctx context.Context, ws *infrav1alpha1.Workspace, runs []infrav1alpha1.Run) error {
	limit := int32(defaultHistoryLimit)
	if ws.Spec.RunHistoryLimit != nil {
		limit = *ws.Spec.RunHistoryLimit
	}
	for _, run := range runsToDelete(runs, limit) {
		if err := r.Delete(ctx, run, client.PropagationPolicy(metav1.DeletePropagationBackground)); client.IgnoreNotFound(err) != nil {
			return err
		}
	}
	return nil
}

func (r *WorkspaceReconciler) event(ws *infrav1alpha1.Workspace, related runtime.Object, eventType, reason, action, note string, args ...any) {
	if r.Recorder != nil {
		r.Recorder.Eventf(ws, related, eventType, reason, action, note, args...)
	}
}

func (r *WorkspaceReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// workspacesReferencing returns a function mapping a Secret or a ConfigMap to the Workspaces referencing it.
func (r *WorkspaceReconciler) workspacesReferencing(kind string) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		return r.workspacesFor(ctx, obj.GetNamespace(), kind+"/"+obj.GetName())
	}
}

func (r *WorkspaceReconciler) workspacesFor(ctx context.Context, namespace, ref string) []reconcile.Request {
	list := &infrav1alpha1.WorkspaceList{}
	if err := r.List(ctx, list, client.InNamespace(namespace), client.MatchingFields{referencedObjectsIndex: ref}); err != nil {
		logf.FromContext(ctx).Error(err, "listing the Workspaces referencing an object", "object", ref)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, ws := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ws)})
	}
	return requests
}

// deletionRequested passes the update setting the deletion timestamp.
var deletionRequested = predicate.Funcs{UpdateFunc: func(e event.UpdateEvent) bool {
	return e.ObjectOld.GetDeletionTimestamp() == nil && e.ObjectNew.GetDeletionTimestamp() != nil
}}

// SetupWithManager sets up the controller with the Manager.
func (r *WorkspaceReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &infrav1alpha1.Workspace{}, referencedObjectsIndex, func(obj client.Object) []string {
		ws := obj.(*infrav1alpha1.Workspace)
		var refs []string
		if cm := ws.Spec.Source.ConfigMap; cm != nil {
			refs = append(refs, "ConfigMap/"+cm.Name)
		}
		for _, ref := range inputRefs(ws) {
			refs = append(refs, ref.String())
		}
		return refs
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		// Status updates are ignored; annotations carry the manual trigger.
		For(&infrav1alpha1.Workspace{}, builder.WithPredicates(predicate.Or(
			predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}, deletionRequested))).
		Owns(&infrav1alpha1.Run{}).
		// Only the metadata of ConfigMaps and Secrets is watched; contents are read on demand.
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.workspacesReferencing("ConfigMap")), builder.OnlyMetadata).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.workspacesReferencing("Secret")), builder.OnlyMetadata).
		Named("workspace").
		Complete(r)
}
