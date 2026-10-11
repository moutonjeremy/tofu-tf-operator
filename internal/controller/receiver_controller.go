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
	"crypto/sha256"
	"encoding/hex"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

const (
	// ReceiverPathIndex indexes Receivers by their webhook path.
	ReceiverPathIndex = ".status.webhookPath"
	// receiverSecretIndex indexes Receivers by the Secret holding their token.
	receiverSecretIndex = ".spec.secretRef.name"
)

// ReceiverWebhookPath derives the path of a Receiver from its token, so that the path
// cannot be guessed and changes with the token.
func ReceiverWebhookPath(token, name, namespace string) string {
	sum := sha256.Sum256([]byte(token + name + namespace))
	return "/hook/" + hex.EncodeToString(sum[:])
}

// IndexReceiverPath is the function of the ReceiverPathIndex.
func IndexReceiverPath(obj client.Object) []string {
	if path := obj.(*infrav1alpha1.Receiver).Status.WebhookPath; path != "" {
		return []string{path}
	}
	return nil
}

// ReceiverReconciler publishes the webhook path of the Receivers.
type ReceiverReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=infra.labbs.fr,resources=receivers,verbs=get;list;watch
// +kubebuilder:rbac:groups=infra.labbs.fr,resources=receivers/status,verbs=get;update;patch

// Reconcile computes the webhook path of a Receiver from its token.
func (r *ReceiverReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	receiver := &infrav1alpha1.Receiver{}
	if err := r.Get(ctx, req.NamespacedName, receiver); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	original := receiver.DeepCopy()

	condition := metav1.Condition{
		Type:               infrav1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             infrav1alpha1.ReasonSucceeded,
		Message:            "the webhook path is in status.webhookPath",
		ObservedGeneration: receiver.Generation,
	}
	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: receiver.Namespace, Name: receiver.Spec.SecretRef.Name}, secret)
	token := strings.TrimSpace(string(secret.Data[infrav1alpha1.ReceiverTokenKey]))
	switch {
	case err != nil:
		receiver.Status.WebhookPath = ""
		condition.Status, condition.Reason, condition.Message = metav1.ConditionFalse, "SecretNotFound", err.Error()
	case token == "":
		receiver.Status.WebhookPath = ""
		condition.Status, condition.Reason = metav1.ConditionFalse, "TokenMissing"
		condition.Message = "the Secret " + secret.Name + " has no " + infrav1alpha1.ReceiverTokenKey + " key"
	default:
		receiver.Status.WebhookPath = ReceiverWebhookPath(token, receiver.Name, receiver.Namespace)
	}
	meta.SetStatusCondition(&receiver.Status.Conditions, condition)
	receiver.Status.ObservedGeneration = receiver.Generation
	if err := r.Status().Patch(ctx, receiver, client.MergeFrom(original)); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	return ctrl.Result{}, nil
}

// receiversForSecret returns the Receivers whose token is in a Secret.
func (r *ReceiverReconciler) receiversForSecret(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &infrav1alpha1.ReceiverList{}
	if err := r.List(ctx, list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{receiverSecretIndex: obj.GetName()}); err != nil {
		logf.FromContext(ctx).Error(err, "listing the Receivers of a Secret")
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, receiver := range list.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&receiver)})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReceiverReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if err := indexer.IndexField(ctx, &infrav1alpha1.Receiver{}, ReceiverPathIndex, IndexReceiverPath); err != nil {
		return err
	}
	if err := indexer.IndexField(ctx, &infrav1alpha1.Receiver{}, receiverSecretIndex, func(obj client.Object) []string {
		return []string{obj.(*infrav1alpha1.Receiver).Spec.SecretRef.Name}
	}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1alpha1.Receiver{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.receiversForSecret), builder.OnlyMetadata).
		Named("receiver").
		Complete(r)
}
