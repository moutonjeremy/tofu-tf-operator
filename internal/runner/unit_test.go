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

package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

const (
	testNamespace = "infra"
	testWorkspace = "vpc"
)

func TestSummarize(t *testing.T) {
	plan := `{"resource_changes": [
		{"change": {"actions": ["create"]}},
		{"change": {"actions": ["update"]}},
		{"change": {"actions": ["delete"]}},
		{"change": {"actions": ["delete", "create"]}},
		{"change": {"actions": ["create", "delete"]}},
		{"change": {"actions": ["no-op"], "importing": {"id": "i-123"}}},
		{"change": {"actions": ["read"]}},
		{"change": {"actions": ["no-op"], "importing": null}}
	]}`
	got, err := Summarize([]byte(plan))
	if err != nil {
		t.Fatal(err)
	}
	want := infrav1alpha1.PlanSummary{Add: 3, Change: 1, Destroy: 3, Import: 1}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestResultEncode(t *testing.T) {
	r := Result{
		Step:    StepApply,
		Error:   "apply failed",
		LogTail: strings.Repeat("a line of log output\n", 1000),
	}
	data := r.Encode()
	if len(data) > MaxResultSize {
		t.Fatalf("encoded result is %d bytes", len(data))
	}
	decoded, ok := DecodeResult(string(data))
	if !ok || decoded.Error != r.Error || !strings.HasPrefix(decoded.LogTail, "a line") {
		t.Fatalf("unexpected decoded result %+v", decoded)
	}

	long := Result{Step: StepPlan, Error: strings.Repeat("x", 2*MaxResultSize)}
	if data := long.Encode(); len(data) > MaxResultSize {
		t.Fatalf("encoded result is %d bytes", len(data))
	}

	if _, ok := DecodeResult("panic: something went wrong"); ok {
		t.Fatal("a crash log must not decode as a result")
	}
}

func TestTailBuffer(t *testing.T) {
	tail := newTailBuffer(16)
	_, _ = tail.Write([]byte("first\n"))
	if tail.String() != "first\n" {
		t.Fatalf("got %q", tail.String())
	}
	_, _ = tail.Write([]byte("second line\nthird\n"))
	if got := tail.String(); got != "third\n" {
		t.Fatalf("got %q", got)
	}
}

func TestVarsFromEnv(t *testing.T) {
	got := varsFromEnv([]string{"TFOP_VAR_region=eu-west-3", "TFOP_VAR_json={\"a\":\"b=c\"}", "TFOP_VAR_=x", "PATH=/bin", "TFOP_RUN_NAME=r"})
	want := map[string]string{"region": "eu-west-3", "json": `{"a":"b=c"}`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	env := engineEnv([]string{"PATH=/bin", "TFOP_VAR_region=x", "TFOP_RUN_NAME=r"})
	for _, kv := range env {
		if strings.HasPrefix(kv, "TFOP_") {
			t.Fatalf("contract variable %q leaked to the engine", kv)
		}
	}
}

func TestDeclaredVariables(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"main.tf":         `variable "a" {}` + "\n" + `resource "x" "y" {}`,
		"more.tofu":       `variable "b" { type = string }`,
		"json.tf.json":    `{"variable": {"c": {}}}`,
		"README.md":       `variable "ignored" {}`,
		"override.tfvars": `a = 1`,
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	declared, err := declaredVariables(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(declared, map[string]bool{"a": true, "b": true, "c": true}) {
		t.Fatalf("got %v", declared)
	}

	vars, ignored := selectVariables(map[string]string{"a": "1", "z": "2"}, declared)
	if !reflect.DeepEqual(vars, map[string]string{"a": "1"}) || !reflect.DeepEqual(ignored, []string{"z"}) {
		t.Fatalf("got %v, ignored %v", vars, ignored)
	}
}

func TestKubernetesBackendOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Namespace: testNamespace, WorkspaceName: testWorkspace, BackendType: infrav1alpha1.BackendKubernetes}
	if err := writeBackendOverride(dir, cfg); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, backendOverrideFile+".tf.json"))
	if err != nil {
		t.Fatal(err)
	}
	var override struct {
		Terraform struct {
			Backend struct {
				Kubernetes map[string]any `json:"kubernetes"`
			} `json:"backend"`
		} `json:"terraform"`
	}
	if err := json.Unmarshal(content, &override); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"secret_suffix": testWorkspace, "namespace": testNamespace, "in_cluster_config": true}
	if !reflect.DeepEqual(override.Terraform.Backend.Kubernetes, want) {
		t.Fatalf("got %v", override.Terraform.Backend.Kubernetes)
	}

	if err := writeBackendOverride(t.TempDir(), Config{BackendType: infrav1alpha1.BackendSource}); err != nil {
		t.Fatal(err)
	}
}

func TestSecretStore(t *testing.T) {
	ctx := context.Background()
	client := fake.NewClientset()
	store := &SecretStore{Secrets: client.CoreV1(), Config: Config{
		Namespace:     testNamespace,
		RunName:       "vpc-abcde",
		RunUID:        "run-uid",
		WorkspaceName: testWorkspace,
		WorkspaceUID:  "ws-uid",
		OutputsSecret: "vpc-outputs",
	}}

	plan := []byte("plan content")
	if err := store.SavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPlan(ctx)
	if err != nil || string(loaded) != string(plan) {
		t.Fatalf("got %q, %v", loaded, err)
	}
	secret, err := client.CoreV1().Secrets(testNamespace).Get(ctx, "vpc-abcde-plan", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if owner := secret.OwnerReferences[0]; owner.Kind != "Run" || owner.UID != "run-uid" {
		t.Fatalf("unexpected owner %+v", owner)
	}

	// Outputs are replaced, not merged.
	if err := store.SaveOutputs(ctx, map[string][]byte{"a": []byte("1"), "b": []byte("2")}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveOutputs(ctx, map[string][]byte{"a": []byte("3")}); err != nil {
		t.Fatal(err)
	}
	secret, err = client.CoreV1().Secrets(testNamespace).Get(ctx, "vpc-outputs", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(secret.Data, map[string][]byte{"a": []byte("3")}) {
		t.Fatalf("unexpected outputs %v", secret.Data)
	}
	if err := store.DeleteOutputs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteOutputs(ctx); err != nil {
		t.Fatalf("deleting missing outputs: %v", err)
	}
}

func TestOutputsToData(t *testing.T) {
	data, err := outputsToData([]byte(`{
		"s": {"sensitive": false, "type": "string", "value": "hello"},
		"n": {"sensitive": false, "type": "number", "value": 3},
		"m": {"sensitive": true, "type": ["map", "string"], "value": {"k": "v"}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]byte{"s": []byte("hello"), "n": []byte("3"), "m": []byte(`{"k":"v"}`)}
	if !reflect.DeepEqual(data, want) {
		t.Fatalf("got %q", data)
	}
}

func TestEngineError(t *testing.T) {
	output := "Initializing...\n\nError: Unsupported argument\n\n  on main.tf line 2\n\nError: tofu plan: exit code 1\n"
	if got := engineError(output); got != "Unsupported argument" {
		t.Fatalf("got %q", got)
	}
	if got := engineError("all good\n"); got != "" {
		t.Fatalf("got %q", got)
	}
}
