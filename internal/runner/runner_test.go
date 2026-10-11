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
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/source"
)

// memStore keeps the artifacts of a Run in memory.
type memStore struct {
	plan    []byte
	outputs map[string][]byte
}

func (m *memStore) SavePlan(_ context.Context, plan []byte) error { m.plan = plan; return nil }

func (m *memStore) LoadPlan(_ context.Context) ([]byte, error) {
	if m.plan == nil {
		return nil, errors.New("no plan")
	}
	return m.plan, nil
}

func (m *memStore) SaveOutputs(_ context.Context, outputs map[string][]byte) error {
	m.outputs = outputs
	return nil
}

func (m *memStore) DeleteOutputs(_ context.Context) error { m.outputs = nil; return nil }

// tofuBin returns the engine binary used by the integration tests (set by `make test`).
func tofuBin(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("TOFU_BIN")
	if bin == "" {
		t.Skip("TOFU_BIN is not set")
	}
	return bin
}

const inlineModule = `
variable "prefix" {
  type = string
}

variable "tags" {
  type = map(string)
}

resource "terraform_data" "this" {
  input = "${var.prefix}-${var.tags["team"]}"
}

output "name" {
  value = terraform_data.this.output
}

output "tags" {
  value = var.tags
}
`

// newTestRunner returns a runner with its own work directory, like a Job would have.
func newTestRunner(t *testing.T, bin, stateFile string, store Store, operation infrav1alpha1.RunOperation) *Runner {
	t.Helper()
	return &Runner{
		Config: Config{
			Namespace:     "default",
			RunName:       "ws-abcde",
			WorkspaceName: "ws",
			Operation:     operation,
			SourceKind:    SourceInline,
			InlineSource:  inlineModule,
			Revision:      source.InlineRevision(inlineModule),
			BackendType:   infrav1alpha1.BackendCustom,
			BackendCustom: fmt.Sprintf("terraform {\n  backend \"local\" {\n    path = %q\n  }\n}\n", stateFile),
			Vars: map[string]string{
				"prefix":     "demo",
				"tags":       `{"team":"platform"}`,
				"undeclared": "ignored",
			},
			OutputsSecret: "ws-outputs",
			WorkDir:       t.TempDir(),
		},
		Store: store,
		Bin:   bin,
		Log:   io.Discard,
	}
}

func TestRunnerLifecycle(t *testing.T) {
	bin := tofuBin(t)
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "terraform.tfstate")
	store := &memStore{}

	// The first plan creates the resource.
	plan := newTestRunner(t, bin, state, store, infrav1alpha1.OperationApply).Run(ctx, StepPlan)
	if plan.Error != "" {
		t.Fatalf("plan failed: %s\n%s", plan.Error, plan.LogTail)
	}
	if !plan.Plan.HasChanges || plan.Plan.Add != 1 || plan.Plan.Digest == "" {
		t.Fatalf("unexpected plan summary %+v", plan.Plan)
	}
	if plan.Revision != source.InlineRevision(inlineModule) {
		t.Fatalf("unexpected revision %q", plan.Revision)
	}

	// The apply runs in another work directory, as in a separate Job.
	applier := newTestRunner(t, bin, state, store, infrav1alpha1.OperationApply)
	applier.Config.PlanDigest = plan.Plan.Digest
	if apply := applier.Run(ctx, StepApply); apply.Error != "" {
		t.Fatalf("apply failed: %s\n%s", apply.Error, apply.LogTail)
	}
	if got := string(store.outputs["name"]); got != "demo-platform" {
		t.Fatalf("unexpected name output %q", got)
	}
	if got := string(store.outputs["tags"]); got != `{"team":"platform"}` {
		t.Fatalf("unexpected tags output %q", got)
	}

	// Nothing changes afterwards.
	replan := newTestRunner(t, bin, state, store, infrav1alpha1.OperationApply).Run(ctx, StepPlan)
	if replan.Error != "" || replan.Plan.HasChanges {
		t.Fatalf("expected no changes, got %+v (%s)", replan.Plan, replan.Error)
	}

	// Destroy removes the resource and the outputs.
	destroyPlan := newTestRunner(t, bin, state, store, infrav1alpha1.OperationDestroy).Run(ctx, StepPlan)
	if destroyPlan.Error != "" || destroyPlan.Plan.Destroy != 1 {
		t.Fatalf("unexpected destroy plan %+v (%s)", destroyPlan.Plan, destroyPlan.Error)
	}
	destroyer := newTestRunner(t, bin, state, store, infrav1alpha1.OperationDestroy)
	destroyer.Config.PlanDigest = destroyPlan.Plan.Digest
	if destroy := destroyer.Run(ctx, StepApply); destroy.Error != "" {
		t.Fatalf("destroy failed: %s\n%s", destroy.Error, destroy.LogTail)
	}
	if store.outputs != nil {
		t.Fatalf("outputs should have been deleted, got %v", store.outputs)
	}
}

func TestRunnerStalePlan(t *testing.T) {
	bin := tofuBin(t)
	ctx := context.Background()
	state := filepath.Join(t.TempDir(), "terraform.tfstate")

	first, second := &memStore{}, &memStore{}
	planA := newTestRunner(t, bin, state, first, infrav1alpha1.OperationApply).Run(ctx, StepPlan)
	planB := newTestRunner(t, bin, state, second, infrav1alpha1.OperationApply).Run(ctx, StepPlan)

	applyA := newTestRunner(t, bin, state, first, infrav1alpha1.OperationApply)
	applyA.Config.PlanDigest = planA.Plan.Digest
	if res := applyA.Run(ctx, StepApply); res.Error != "" {
		t.Fatalf("apply failed: %s", res.Error)
	}

	applyB := newTestRunner(t, bin, state, second, infrav1alpha1.OperationApply)
	applyB.Config.PlanDigest = planB.Plan.Digest
	res := applyB.Run(ctx, StepApply)
	if res.Error == "" || !res.StalePlan {
		t.Fatalf("expected a stale plan error, got %+v", res)
	}
}

func TestRunnerRejectsAnotherPlan(t *testing.T) {
	bin := tofuBin(t)
	store := &memStore{}
	state := filepath.Join(t.TempDir(), "terraform.tfstate")
	plan := newTestRunner(t, bin, state, store, infrav1alpha1.OperationApply).Run(context.Background(), StepPlan)
	if plan.Error != "" {
		t.Fatalf("plan failed: %s", plan.Error)
	}

	applier := newTestRunner(t, bin, state, store, infrav1alpha1.OperationApply)
	applier.Config.PlanDigest = Digest([]byte("another plan"))
	res := applier.Run(context.Background(), StepApply)
	if !strings.Contains(res.Error, "is not the planned one") {
		t.Fatalf("expected a digest mismatch, got %q", res.Error)
	}
}

func TestFetchGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	ctx := context.Background()
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--initial-branch=main")
	if err := os.MkdirAll(filepath.Join(repo, "envs", "dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "envs", "dev", "main.tf"), []byte("# v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "--quiet", "-m", "v1")
	commit := git("rev-parse", "HEAD")
	// A later commit must not be the one checked out.
	if err := os.WriteFile(filepath.Join(repo, "envs", "dev", "main.tf"), []byte("# v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "--quiet", "-am", "v2")

	r := &Runner{
		Config: Config{
			SourceKind:        SourceGit,
			GitURL:            "file://" + repo,
			GitPath:           "envs/dev",
			Revision:          source.GitRevision("main", commit),
			GitCredentialsDir: t.TempDir(),
			WorkDir:           t.TempDir(),
		},
		Log: io.Discard,
	}
	if err := r.prepare(); err != nil {
		t.Fatal(err)
	}
	dir, revision, err := r.fetchSource(ctx)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "main.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# v1\n" || revision != r.Config.Revision {
		t.Fatalf("unexpected checkout: %q at %s", content, revision)
	}
}

func TestFetchConfigMap(t *testing.T) {
	// The kubelet mounts the keys as symlinks to a hidden timestamped directory.
	mount := t.TempDir()
	data := filepath.Join(mount, "..2026_10_11_12_00_00.000000000")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"main.tf": []byte("# main\n"), "variables.tf": []byte("# vars\n")}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(data, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Base(data), filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	for name := range files {
		if err := os.Symlink(filepath.Join("..data", name), filepath.Join(mount, name)); err != nil {
			t.Fatal(err)
		}
	}

	r := &Runner{Config: Config{SourceKind: SourceConfigMap, ConfigMapDir: mount, WorkDir: t.TempDir()}}
	dir, revision, err := r.fetchSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if revision != source.FilesRevision(files) {
		t.Fatalf("revision %s does not match the ConfigMap content", revision)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != len(files) {
		t.Fatalf("expected %d files, got %v", len(files), entries)
	}
}
