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
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

var _ = Describe("Receiver", func() {
	It("publishes a webhook path derived from the token", func() {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "receiver-"}}
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
		key := types.NamespacedName{Namespace: ns.Name, Name: "push"}
		reconciler := &ReceiverReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		reconcileReceiver := func() *infrav1alpha1.Receiver {
			GinkgoHelper()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			receiver := &infrav1alpha1.Receiver{}
			Expect(k8sClient.Get(ctx, key, receiver)).To(Succeed())
			return receiver
		}

		Expect(k8sClient.Create(ctx, &infrav1alpha1.Receiver{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec:       infrav1alpha1.ReceiverSpec{Type: infrav1alpha1.ReceiverGitHub, SecretRef: corev1.LocalObjectReference{Name: "hook"}},
		})).To(Succeed())

		By("waiting for its Secret")
		receiver := reconcileReceiver()
		Expect(receiver.Status.WebhookPath).To(BeEmpty())
		Expect(meta.IsStatusConditionFalse(receiver.Status.Conditions, infrav1alpha1.ConditionReady)).To(BeTrue())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "hook", Namespace: key.Namespace},
			StringData: map[string]string{infrav1alpha1.ReceiverTokenKey: "first"},
		}
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())
		receiver = reconcileReceiver()
		Expect(receiver.Status.WebhookPath).To(Equal(ReceiverWebhookPath("first", key.Name, key.Namespace)))
		Expect(meta.IsStatusConditionTrue(receiver.Status.Conditions, infrav1alpha1.ConditionReady)).To(BeTrue())

		By("changing the path with the token")
		secret.StringData = map[string]string{infrav1alpha1.ReceiverTokenKey: "second"}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())
		Expect(reconcileReceiver().Status.WebhookPath).To(Equal(ReceiverWebhookPath("second", key.Name, key.Namespace)))
	})
})
