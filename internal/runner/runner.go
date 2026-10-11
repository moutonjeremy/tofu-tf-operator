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
	"path/filepath"
	"slices"
	"strings"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// logTailSize is how much of the output is kept to explain a failure.
const logTailSize = 8 * 1024

// Runner executes the steps of a Run.
type Runner struct {
	Config Config
	Store  Store
	// Bin is the path of the engine binary.
	Bin string
	// Log receives the output of the step.
	Log io.Writer

	env  []string
	home string
	tail *tailBuffer
}

// Run executes a step and reports its outcome.
func (r *Runner) Run(ctx context.Context, step Step) Result {
	r.tail = newTailBuffer(logTailSize)
	r.Log = io.MultiWriter(r.Log, r.tail)
	result := Result{Step: step}

	err := r.prepare()
	if err == nil {
		switch step {
		case StepPlan:
			err = r.plan(ctx, &result)
		case StepApply:
			err = r.apply(ctx, &result)
		default:
			err = fmt.Errorf("unknown step %q", step)
		}
	}
	if err != nil {
		result.LogTail = r.tail.String()
		result.Error = err.Error()
		// The first error reported by the engine explains the failure better than its exit code.
		if summary := engineError(result.LogTail); summary != "" {
			result.Error += ": " + summary
		}
		result.StalePlan = strings.Contains(result.LogTail, "Saved plan is stale")
		_, _ = fmt.Fprintf(r.Log, "\nError: %v\n", err)
	}
	return result
}

// engineError returns the first error reported by the engine in its output.
func engineError(output string) string {
	for line := range strings.Lines(output) {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "Error: "); ok {
			return msg
		}
	}
	return ""
}

func (r *Runner) prepare() error {
	r.home = filepath.Join(r.Config.WorkDir, "home")
	if err := os.MkdirAll(r.home, 0o700); err != nil {
		return err
	}
	r.env = append(engineEnv(os.Environ()), "HOME="+r.home)
	return nil
}

func (r *Runner) plan(ctx context.Context, result *Result) error {
	tofu, err := r.init(ctx, result)
	if err != nil {
		return err
	}

	planFile := filepath.Join(r.Config.WorkDir, "tfplan")
	destroy := r.Config.Operation == infrav1alpha1.OperationDestroy
	hasChanges, err := tofu.Plan(ctx, planFile, destroy, r.variables(tofu.Dir))
	if err != nil {
		return err
	}
	if !hasChanges {
		result.Plan = &infrav1alpha1.PlanSummary{}
		return nil
	}

	planJSON, err := tofu.ShowJSON(ctx, planFile)
	if err != nil {
		return err
	}
	summary, err := Summarize(planJSON)
	if err != nil {
		return err
	}
	plan, err := os.ReadFile(planFile)
	if err != nil {
		return err
	}
	summary.HasChanges = true
	summary.Digest = Digest(plan)
	if err := r.Store.SavePlan(ctx, plan); err != nil {
		return fmt.Errorf("saving the plan: %w", err)
	}
	result.Plan = &summary
	return nil
}

func (r *Runner) apply(ctx context.Context, result *Result) error {
	if r.Config.PlanDigest == "" {
		return errors.New("no plan digest to apply")
	}
	tofu, err := r.init(ctx, result)
	if err != nil {
		return err
	}
	if result.Revision != r.Config.Revision {
		_, _ = fmt.Fprintf(r.Log, "warning: the source changed since the plan (%s, planned %s); applying the plan as reviewed\n",
			result.Revision, r.Config.Revision)
	}

	plan, err := r.Store.LoadPlan(ctx)
	if err != nil {
		return err
	}
	if digest := Digest(plan); digest != r.Config.PlanDigest {
		return fmt.Errorf("the stored plan (%s) is not the planned one (%s)", digest, r.Config.PlanDigest)
	}
	planFile := filepath.Join(r.Config.WorkDir, "tfplan")
	if err := os.WriteFile(planFile, plan, 0o600); err != nil {
		return err
	}
	if err := tofu.Apply(ctx, planFile); err != nil {
		return err
	}

	if r.Config.OutputsSecret == "" {
		return nil
	}
	if r.Config.Operation == infrav1alpha1.OperationDestroy {
		return r.Store.DeleteOutputs(ctx)
	}
	outputJSON, err := tofu.OutputJSON(ctx)
	if err != nil {
		return err
	}
	outputs, err := outputsToData(outputJSON)
	if err != nil {
		return err
	}
	if err := r.Store.SaveOutputs(ctx, outputs); err != nil {
		return fmt.Errorf("saving the outputs: %w", err)
	}
	_, _ = fmt.Fprintf(r.Log, "outputs written to Secret %s\n", r.Config.OutputsSecret)
	return nil
}

// init fetches the source, configures the backend and runs init.
func (r *Runner) init(ctx context.Context, result *Result) (*Tofu, error) {
	dir, revision, err := r.fetchSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching the source: %w", err)
	}
	result.Revision = revision
	if err := writeBackendOverride(dir, r.Config); err != nil {
		return nil, fmt.Errorf("configuring the backend: %w", err)
	}
	tofu := &Tofu{Bin: r.Bin, Dir: dir, Env: r.env, Out: r.Log}
	return tofu, tofu.Init(ctx)
}

// variables returns the input variables declared by the root module.
func (r *Runner) variables(dir string) map[string]string {
	if len(r.Config.Vars) == 0 {
		return nil
	}
	declared, err := declaredVariables(dir)
	if err != nil {
		// The engine reports configuration errors better; let it do so.
		return r.Config.Vars
	}
	vars, ignored := selectVariables(r.Config.Vars, declared)
	if len(ignored) > 0 {
		slices.Sort(ignored)
		_, _ = fmt.Fprintf(r.Log, "ignoring variables not declared by the module: %s\n", strings.Join(ignored, ", "))
	}
	return vars
}
