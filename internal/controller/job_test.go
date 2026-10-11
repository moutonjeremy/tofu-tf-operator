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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/runner"
)

func TestStepJobName(t *testing.T) {
	if got := stepJobName("vpc-12", runner.StepPlan); got != "vpc-12-plan" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("a", 60) + "-12"
	plan, apply := stepJobName(long, runner.StepPlan), stepJobName(long, runner.StepApply)
	if len(plan) > maxNameLength || len(apply) > maxNameLength || !strings.HasSuffix(apply, "-apply") {
		t.Fatalf("unexpected names %q, %q", plan, apply)
	}
	if stepJobName(strings.Repeat("a", 60)+"-13", runner.StepPlan) == plan {
		t.Fatal("two Runs share a Job name")
	}
}

func TestVariableValue(t *testing.T) {
	for raw, want := range map[string]string{
		`"eu-west-3"`:                  "eu-west-3",
		`3`:                            "3",
		`12345678901234567890`:         "12345678901234567890",
		`true`:                         "true",
		`{ "team": "platform" }`:       `{"team":"platform"}`,
		`["a", "b"]`:                   `["a","b"]`,
		`"{\"looks\": \"like json\"}"`: `{"looks": "like json"}`,
	} {
		got, err := variableValue([]byte(raw))
		if err != nil || got != want {
			t.Errorf("variableValue(%s) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func envValue(env []corev1.EnvVar, name string) (corev1.EnvVar, bool) {
	var found corev1.EnvVar
	ok := false
	// The last entry wins, as in Kubernetes.
	for _, e := range env {
		if e.Name == name {
			found, ok = e, true
		}
	}
	return found, ok
}

func hasMount(c corev1.Container, name, path string) bool {
	for _, m := range c.VolumeMounts {
		if m.Name == name && m.MountPath == path {
			return true
		}
	}
	return false
}

func TestBuildStepJob(t *testing.T) {
	ws := &infrav1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "infra", UID: "ws-uid"},
		Spec: infrav1alpha1.WorkspaceSpec{
			Engine: infrav1alpha1.EngineSpec{Type: infrav1alpha1.EngineTofu, Version: "1.13.1"},
			Source: infrav1alpha1.SourceSpec{Git: &infrav1alpha1.GitSource{
				URL: "https://github.com/acme/infra.git", Path: "envs/prod", SecretRef: &corev1.LocalObjectReference{Name: "git-creds"},
			}},
			Vars: []infrav1alpha1.Variable{
				{Name: "region", Value: &apiextensionsv1.JSON{Raw: []byte(`"eu-west-3"`)}},
				{Name: "tags", Value: &apiextensionsv1.JSON{Raw: []byte(`{"team": "platform"}`)}},
				{Name: "password", ValueFrom: &infrav1alpha1.VariableSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "db"}, Key: "password",
				}}},
			},
			VarsFrom: []infrav1alpha1.VarsFromSource{{Kind: "Secret", Name: "tfvars"}, {Kind: "ConfigMap", Name: "common", Optional: true}},
			Env: []corev1.EnvVar{
				{Name: "TF_LOG", Value: "INFO"},
				{Name: runner.EnvNamespace, Value: "spoofed"},
			},
			EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "aws"}}}},
			Backend: infrav1alpha1.BackendSpec{Type: infrav1alpha1.BackendKubernetes},
			Outputs: &infrav1alpha1.OutputsSpec{SecretName: "vpc-outputs"},
			Runner:  infrav1alpha1.RunnerSpec{Timeout: &metav1.Duration{Duration: 30 * time.Minute}},
		},
	}
	run := &infrav1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc-3", Namespace: "infra", UID: "run-uid"},
		Spec:       infrav1alpha1.RunSpec{Operation: infrav1alpha1.OperationApply, Revision: "main@sha1:abc"},
		Status:     infrav1alpha1.RunStatus{Plan: &infrav1alpha1.PlanSummary{HasChanges: true, Digest: "sha256:plan"}},
	}

	job, err := buildStepJob(run, ws, runner.StepPlan, "runner:default")
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != "vpc-3-plan" || job.Namespace != "infra" || job.Labels[infrav1alpha1.RunLabel] != "vpc-3" {
		t.Fatalf("unexpected metadata %+v", job.ObjectMeta)
	}
	if *job.Spec.BackoffLimit != 0 || *job.Spec.ActiveDeadlineSeconds != 1800 {
		t.Fatalf("unexpected job spec %+v", job.Spec)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != DefaultRunnerServiceAccount || pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("unexpected pod spec %+v", pod)
	}
	c := pod.Containers[0]
	if c.Image != "runner:default" || c.Args[0] != "plan" || c.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("unexpected container %+v", c)
	}

	for name, want := range map[string]string{
		runner.EnvNamespace:         "infra",
		runner.EnvRunName:           "vpc-3",
		runner.EnvRunUID:            "run-uid",
		runner.EnvWorkspaceUID:      "ws-uid",
		runner.EnvSourceKind:        "git",
		runner.EnvGitURL:            "https://github.com/acme/infra.git",
		runner.EnvGitPath:           "envs/prod",
		runner.EnvRevision:          "main@sha1:abc",
		runner.EnvEngineVersion:     "1.13.1",
		runner.EnvOutputsSecret:     "vpc-outputs",
		runner.VarPrefix + "region": "eu-west-3",
		runner.VarPrefix + "tags":   `{"team":"platform"}`,
		"TF_LOG":                    "INFO",
	} {
		if e, ok := envValue(c.Env, name); !ok || e.Value != want {
			t.Errorf("env %s = %q, want %q", name, e.Value, want)
		}
	}
	if e, _ := envValue(c.Env, runner.VarPrefix+"password"); e.ValueFrom == nil || e.ValueFrom.SecretKeyRef.Name != "db" {
		t.Errorf("the password variable must reference the Secret, got %+v", e)
	}
	if _, ok := envValue(c.Env, runner.EnvPlanDigest); ok {
		t.Error("the plan digest is only passed to the apply step")
	}

	if len(c.EnvFrom) != 3 || c.EnvFrom[1].Prefix != runner.VarPrefix || c.EnvFrom[1].SecretRef.Name != "tfvars" ||
		c.EnvFrom[2].ConfigMapRef.Name != "common" || !*c.EnvFrom[2].ConfigMapRef.Optional {
		t.Fatalf("unexpected envFrom %+v", c.EnvFrom)
	}
	mounted := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounted[m.Name] = m.MountPath
	}
	if mounted["git-credentials"] != runner.DefaultGitCredentialsDir || mounted["workspace"] != runner.DefaultWorkDir {
		t.Fatalf("unexpected mounts %v", mounted)
	}

	ws.Spec.Runner.Image = "runner:custom"
	ws.Spec.Runner.ServiceAccountName = "custom-sa"
	apply, err := buildStepJob(run, ws, runner.StepApply, "runner:default")
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := envValue(apply.Spec.Template.Spec.Containers[0].Env, runner.EnvPlanDigest); e.Value != "sha256:plan" {
		t.Errorf("the apply step must receive the plan digest, got %q", e.Value)
	}
	if apply.Spec.Template.Spec.Containers[0].Image != "runner:custom" || apply.Spec.Template.Spec.ServiceAccountName != "custom-sa" {
		t.Error("the runner settings of the Workspace must be used")
	}
}

func TestBuildStepJobPlanStorage(t *testing.T) {
	ws := &infrav1alpha1.Workspace{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: "infra"},
		Spec:       infrav1alpha1.WorkspaceSpec{Source: infrav1alpha1.SourceSpec{Inline: "# empty"}},
	}
	run := &infrav1alpha1.Run{ObjectMeta: metav1.ObjectMeta{Name: "vpc-3", Namespace: "infra"}}
	job, err := buildStepJob(run, ws, runner.StepPlan, "runner:default")
	if err != nil {
		t.Fatal(err)
	}
	c := job.Spec.Template.Spec.Containers[0]
	if _, ok := envValue(c.Env, runner.EnvS3Bucket); ok {
		t.Error("no S3 settings without an S3 plan storage")
	}
	if e, _ := envValue(c.Env, runner.EnvPlanStorage); e.Value != "" {
		t.Errorf("the default plan storage is left to the runner, got %q", e.Value)
	}

	ws.Spec.PlanStorage = infrav1alpha1.PlanStorageSpec{Type: infrav1alpha1.PlanStorageS3, S3: &infrav1alpha1.S3PlanStorage{
		Bucket: "plans", Prefix: "tofu/", Endpoint: "https://minio.example.com", PathStyle: true,
		SecretRef: &corev1.LocalObjectReference{Name: "s3-creds"},
	}}
	s3Job, err := buildStepJob(run, ws, runner.StepPlan, "runner:default")
	if err != nil {
		t.Fatal(err)
	}
	s3Container := s3Job.Spec.Template.Spec.Containers[0]
	for name, want := range map[string]string{
		runner.EnvPlanStorage: "S3", runner.EnvS3Bucket: "plans", runner.EnvS3Prefix: "tofu/",
		runner.EnvS3Endpoint: "https://minio.example.com", runner.EnvS3PathStyle: "true",
	} {
		if e, _ := envValue(s3Container.Env, name); e.Value != want {
			t.Errorf("env %s = %q, want %q", name, e.Value, want)
		}
	}
	if !hasMount(s3Container, "plan-storage", runner.DefaultPlanStorageDir) {
		t.Error("the S3 credentials must be mounted")
	}

}
