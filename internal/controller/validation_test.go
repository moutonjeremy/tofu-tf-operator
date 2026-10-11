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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

const testNamespace = "default"

// These tests exercise the OpenAPI and CEL validation of the CRDs against a real API server.
var _ = Describe("CRD validation", func() {
	ctx := context.Background()

	newWorkspace := func(name string) *infrav1alpha1.Workspace {
		return &infrav1alpha1.Workspace{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
			Spec: infrav1alpha1.WorkspaceSpec{
				Source: infrav1alpha1.SourceSpec{Inline: `output "hello" { value = "world" }`},
			},
		}
	}

	Context("Workspace", func() {
		It("applies defaults", func() {
			ws := newWorkspace("defaults")
			Expect(k8sClient.Create(ctx, ws)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, ws)

			Expect(ws.Spec.Engine.Type).To(Equal(infrav1alpha1.EngineTofu))
			Expect(ws.Spec.Backend.Type).To(Equal(infrav1alpha1.BackendKubernetes))
			Expect(ws.Spec.ApprovalMode).To(Equal(infrav1alpha1.ApprovalAuto))
			Expect(ws.Spec.Runner.Timeout.Duration).To(Equal(time.Hour))
			Expect(ws.Spec.PlanStorage.Type).To(Equal(infrav1alpha1.PlanStorageSecret))
			Expect(*ws.Spec.RunHistoryLimit).To(BeEquivalentTo(10))
		})

		It("accepts the OCI source and the external plan storages", func() {
			ws := newWorkspace("oci-postgres")
			ws.Spec.Source = infrav1alpha1.SourceSpec{OCI: &infrav1alpha1.OCISource{URL: "oci://ghcr.io/acme/vpc", Ref: infrav1alpha1.OCIRef{Tag: "v1"}}}
			ws.Spec.PlanStorage = infrav1alpha1.PlanStorageSpec{
				Type:     infrav1alpha1.PlanStoragePostgres,
				Postgres: &infrav1alpha1.PostgresPlanStorage{SecretRef: corev1.LocalObjectReference{Name: "db"}},
			}
			Expect(k8sClient.Create(ctx, ws)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, ws)
			Expect(ws.Spec.Source.OCI.Interval.Duration).To(Equal(time.Minute))
			Expect(ws.Spec.PlanStorage.Postgres.Table).To(Equal("tofu_plans"))

			s3 := newWorkspace("s3")
			s3.Spec.PlanStorage = infrav1alpha1.PlanStorageSpec{Type: infrav1alpha1.PlanStorageS3, S3: &infrav1alpha1.S3PlanStorage{Bucket: "plans"}}
			Expect(k8sClient.Create(ctx, s3)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, s3)
		})

		It("defaults the git polling interval", func() {
			ws := newWorkspace("git")
			ws.Spec.Source = infrav1alpha1.SourceSpec{Git: &infrav1alpha1.GitSource{
				URL: "https://github.com/acme/infra.git",
				Ref: infrav1alpha1.GitRef{Branch: "main"},
			}}
			Expect(k8sClient.Create(ctx, ws)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, ws)

			Expect(ws.Spec.Source.Git.Interval.Duration).To(Equal(time.Minute))
		})

		DescribeTable("rejects invalid specs",
			func(mutate func(*infrav1alpha1.Workspace)) {
				ws := newWorkspace("invalid")
				mutate(ws)
				err := k8sClient.Create(ctx, ws)
				Expect(errors.IsInvalid(err)).To(BeTrue(), "expected an Invalid error, got %v", err)
			},
			Entry("no source", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Source = infrav1alpha1.SourceSpec{}
			}),
			Entry("two sources", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Source.ConfigMap = &infrav1alpha1.ConfigMapSource{Name: "module"}
			}),
			Entry("terraform without version", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Engine.Type = infrav1alpha1.EngineTerraform
			}),
			Entry("custom backend without configuration", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Backend.Type = infrav1alpha1.BackendCustom
			}),
			Entry("backend configuration without custom type", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Backend.Custom = `terraform { backend "s3" {} }`
			}),
			Entry("git path escaping the repository", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Source = infrav1alpha1.SourceSpec{Git: &infrav1alpha1.GitSource{URL: "https://x/y.git", Path: "../etc"}}
			}),
			Entry("git and oci sources", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Source = infrav1alpha1.SourceSpec{
					Git: &infrav1alpha1.GitSource{URL: "https://x/y.git"},
					OCI: &infrav1alpha1.OCISource{URL: "oci://ghcr.io/acme/vpc"},
				}
			}),
			Entry("oci url without scheme", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Source = infrav1alpha1.SourceSpec{OCI: &infrav1alpha1.OCISource{URL: "ghcr.io/acme/vpc"}}
			}),
			Entry("oci ref with tag and digest", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Source = infrav1alpha1.SourceSpec{OCI: &infrav1alpha1.OCISource{
					URL: "oci://ghcr.io/acme/vpc",
					Ref: infrav1alpha1.OCIRef{Tag: "v1", Digest: "sha256:" + strings.Repeat("a", 64)},
				}}
			}),
			Entry("S3 plan storage without configuration", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.PlanStorage.Type = infrav1alpha1.PlanStorageS3
			}),
			Entry("S3 configuration with the Secret plan storage", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.PlanStorage.S3 = &infrav1alpha1.S3PlanStorage{Bucket: "plans"}
			}),
			Entry("invalid Postgres table name", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.PlanStorage = infrav1alpha1.PlanStorageSpec{Type: infrav1alpha1.PlanStoragePostgres, Postgres: &infrav1alpha1.PostgresPlanStorage{
					SecretRef: corev1.LocalObjectReference{Name: "db"}, Table: "plans; drop table x",
				}}
			}),
			Entry("git ref with branch and tag", func(ws *infrav1alpha1.Workspace) {
				ws.Spec.Source = infrav1alpha1.SourceSpec{Git: &infrav1alpha1.GitSource{
					URL: "https://x/y.git",
					Ref: infrav1alpha1.GitRef{Branch: "main", Tag: "v1"},
				}}
			}),
		)
	})

	Context("Run", func() {
		It("keeps its spec immutable except approval", func() {
			run := &infrav1alpha1.Run{
				ObjectMeta: metav1.ObjectMeta{Name: "immutable", Namespace: testNamespace},
				Spec: infrav1alpha1.RunSpec{
					WorkspaceRef: corev1.LocalObjectReference{Name: "ws"},
					Trigger:      infrav1alpha1.TriggerManual,
					Revision:     "abc",
				},
			}
			Expect(k8sClient.Create(ctx, run)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, run)
			Expect(run.Spec.Operation).To(Equal(infrav1alpha1.OperationApply))

			run.Spec.ApprovedPlan = "not a digest"
			Expect(errors.IsInvalid(k8sClient.Update(ctx, run))).To(BeTrue())
			run.Spec.ApprovedPlan = "sha256:" + strings.Repeat("0", 64)
			Expect(k8sClient.Update(ctx, run)).To(Succeed())

			run.Spec.Revision = "def"
			Expect(errors.IsInvalid(k8sClient.Update(ctx, run))).To(BeTrue())
		})
	})

	Context("Receiver", func() {
		It("requires a type and a Secret", func() {
			receiver := &infrav1alpha1.Receiver{
				ObjectMeta: metav1.ObjectMeta{Name: "push", Namespace: testNamespace},
				Spec:       infrav1alpha1.ReceiverSpec{Type: "bitbucket", SecretRef: corev1.LocalObjectReference{Name: "token"}},
			}
			Expect(errors.IsInvalid(k8sClient.Create(ctx, receiver))).To(BeTrue())
			receiver.Spec.Type = infrav1alpha1.ReceiverGitHub
			Expect(k8sClient.Create(ctx, receiver)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, receiver)
		})
	})

	Context("NotificationProvider", func() {
		It("requires an address or a secret", func() {
			provider := &infrav1alpha1.NotificationProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: testNamespace},
			}
			Expect(errors.IsInvalid(k8sClient.Create(ctx, provider))).To(BeTrue())

			provider.Spec.Address = "https://hooks.example.com/tofu"
			Expect(k8sClient.Create(ctx, provider)).To(Succeed())
			DeferCleanup(k8sClient.Delete, ctx, provider)
			Expect(provider.Spec.Type).To(Equal(infrav1alpha1.ProviderWebhook))
		})
	})
})
