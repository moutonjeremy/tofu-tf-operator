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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// gracePeriod is how long the engine is given to stop after an interrupt,
// so it can persist the state and release its lock.
const gracePeriod = 2 * time.Minute

// Tofu runs OpenTofu or Terraform commands in a working directory.
type Tofu struct {
	// Bin is the path of the tofu or terraform binary.
	Bin string
	// Dir is the root module directory.
	Dir string
	// Env is the environment of the commands.
	Env []string
	// Out receives the human readable output of the commands.
	Out io.Writer
}

// Init runs `init`.
func (t *Tofu) Init(ctx context.Context) error {
	_, err := t.run(ctx, t.Out, "init", "-input=false", "-no-color")
	return err
}

// Plan runs `plan` and writes the plan to planFile. It reports whether the plan has changes.
func (t *Tofu) Plan(ctx context.Context, planFile string, destroy bool, vars map[string]string) (bool, error) {
	args := []string{"plan", "-input=false", "-no-color", "-detailed-exitcode", "-out=" + planFile}
	if destroy {
		args = append(args, "-destroy")
	}
	for _, name := range slices.Sorted(maps.Keys(vars)) {
		args = append(args, "-var", name+"="+vars[name])
	}

	code, err := t.run(ctx, t.Out, args...)
	switch {
	case code == 2:
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// ShowJSON returns the JSON representation of a plan file.
func (t *Tofu) ShowJSON(ctx context.Context, planFile string) ([]byte, error) {
	var out bytes.Buffer
	if _, err := t.run(ctx, &out, "show", "-json", "-no-color", planFile); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Apply applies a plan file.
func (t *Tofu) Apply(ctx context.Context, planFile string) error {
	_, err := t.run(ctx, t.Out, "apply", "-input=false", "-no-color", planFile)
	return err
}

// OutputJSON returns the outputs of the root module as JSON.
func (t *Tofu) OutputJSON(ctx context.Context) ([]byte, error) {
	var out bytes.Buffer
	if _, err := t.run(ctx, &out, "output", "-json", "-no-color"); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// run executes a command and returns its exit code. stderr always goes to Out.
func (t *Tofu) run(ctx context.Context, stdout io.Writer, args ...string) (int, error) {
	_, _ = fmt.Fprintf(t.Out, "\n$ %s %s\n", t.Bin, strings.Join(redactVars(args), " "))

	cmd := exec.CommandContext(ctx, t.Bin, args...)
	cmd.Dir = t.Dir
	cmd.Env = t.Env
	cmd.Stdout = stdout
	cmd.Stderr = t.Out
	// Interrupt instead of kill, so the engine persists the state and releases the lock.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = gracePeriod

	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	name := filepath.Base(t.Bin)
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), fmt.Errorf("%s %s: exit code %d", name, args[0], exitErr.ExitCode())
	}
	return -1, fmt.Errorf("%s %s: %w", name, args[0], err)
}

// redactVars hides the values of -var arguments in the logs.
func redactVars(args []string) []string {
	out := make([]string, len(args))
	for i, arg := range args {
		out[i] = arg
		if i > 0 && args[i-1] == "-var" {
			name, _, _ := strings.Cut(arg, "=")
			out[i] = name + "=<redacted>"
		}
	}
	return out
}
