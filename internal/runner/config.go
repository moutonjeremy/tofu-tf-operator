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

// Package runner executes the plan and apply steps of a Run inside a Job.
package runner

import (
	"fmt"
	"os"
	"strings"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// Environment variables set by the Run controller on the runner container.
const (
	EnvNamespace     = "TFOP_NAMESPACE"
	EnvRunName       = "TFOP_RUN_NAME"
	EnvRunUID        = "TFOP_RUN_UID"
	EnvWorkspaceName = "TFOP_WORKSPACE_NAME"
	EnvWorkspaceUID  = "TFOP_WORKSPACE_UID"
	EnvOperation     = "TFOP_OPERATION"
	EnvEngine        = "TFOP_ENGINE"
	EnvEngineVersion = "TFOP_ENGINE_VERSION"
	EnvSourceKind    = "TFOP_SOURCE_KIND"
	EnvRevision      = "TFOP_REVISION"
	EnvGitURL        = "TFOP_GIT_URL"
	EnvGitPath       = "TFOP_GIT_PATH"
	EnvInlineSource  = "TFOP_INLINE_SOURCE"
	EnvBackendType   = "TFOP_BACKEND_TYPE"
	EnvBackendCustom = "TFOP_BACKEND_CUSTOM"
	EnvPlanDigest    = "TFOP_PLAN_DIGEST"
	EnvOutputsSecret = "TFOP_OUTPUTS_SECRET"

	// VarPrefix prefixes the environment variables holding input variables.
	VarPrefix = "TFOP_VAR_"
	// envPrefix prefixes every variable of the contract; they are not passed to the engine.
	envPrefix = "TFOP_"
)

// Paths mounted by the Run controller in the runner container.
const (
	DefaultWorkDir           = "/workspace"
	DefaultGitCredentialsDir = "/etc/tofu-runner/git"
	DefaultConfigMapDir      = "/etc/tofu-runner/source"
)

// SourceKind is where the configuration is fetched from.
type SourceKind string

const (
	SourceGit       SourceKind = "git"
	SourceConfigMap SourceKind = "configmap"
	SourceInline    SourceKind = "inline"
)

// Step is the step executed by the runner.
type Step string

const (
	StepPlan  Step = "plan"
	StepApply Step = "apply"
)

// Config is everything the runner needs to execute a step.
type Config struct {
	Namespace     string
	RunName       string
	RunUID        string
	WorkspaceName string
	WorkspaceUID  string

	Operation     infrav1alpha1.RunOperation
	Engine        infrav1alpha1.EngineType
	EngineVersion string

	SourceKind        SourceKind
	Revision          string
	GitURL            string
	GitPath           string
	GitCredentialsDir string
	InlineSource      string
	ConfigMapDir      string

	BackendType   infrav1alpha1.BackendType
	BackendCustom string

	// Vars are the input variables, by name.
	Vars map[string]string

	// PlanDigest is the digest of the plan to apply.
	PlanDigest string
	// OutputsSecret receives the outputs when not empty.
	OutputsSecret string

	WorkDir string
}

// ConfigFromEnv reads the configuration set by the Run controller.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Namespace:         os.Getenv(EnvNamespace),
		RunName:           os.Getenv(EnvRunName),
		RunUID:            os.Getenv(EnvRunUID),
		WorkspaceName:     os.Getenv(EnvWorkspaceName),
		WorkspaceUID:      os.Getenv(EnvWorkspaceUID),
		Operation:         infrav1alpha1.RunOperation(os.Getenv(EnvOperation)),
		Engine:            infrav1alpha1.EngineType(os.Getenv(EnvEngine)),
		EngineVersion:     os.Getenv(EnvEngineVersion),
		SourceKind:        SourceKind(os.Getenv(EnvSourceKind)),
		Revision:          os.Getenv(EnvRevision),
		GitURL:            os.Getenv(EnvGitURL),
		GitPath:           os.Getenv(EnvGitPath),
		GitCredentialsDir: DefaultGitCredentialsDir,
		InlineSource:      os.Getenv(EnvInlineSource),
		ConfigMapDir:      DefaultConfigMapDir,
		BackendType:       infrav1alpha1.BackendType(os.Getenv(EnvBackendType)),
		BackendCustom:     os.Getenv(EnvBackendCustom),
		Vars:              varsFromEnv(os.Environ()),
		PlanDigest:        os.Getenv(EnvPlanDigest),
		OutputsSecret:     os.Getenv(EnvOutputsSecret),
		WorkDir:           DefaultWorkDir,
	}
	if cfg.Operation == "" {
		cfg.Operation = infrav1alpha1.OperationApply
	}
	if cfg.Engine == "" {
		cfg.Engine = infrav1alpha1.EngineTofu
	}
	if cfg.BackendType == "" {
		cfg.BackendType = infrav1alpha1.BackendKubernetes
	}

	var missing []string
	for name, value := range map[string]string{
		EnvNamespace:     cfg.Namespace,
		EnvRunName:       cfg.RunName,
		EnvRunUID:        cfg.RunUID,
		EnvWorkspaceName: cfg.WorkspaceName,
		EnvSourceKind:    string(cfg.SourceKind),
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return cfg, fmt.Errorf("missing environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}

// varsFromEnv extracts the input variables from environment entries.
func varsFromEnv(environ []string) map[string]string {
	vars := map[string]string{}
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, VarPrefix) || name == VarPrefix {
			continue
		}
		vars[strings.TrimPrefix(name, VarPrefix)] = value
	}
	return vars
}

// engineEnv is the environment of the engine: the runner's one without the contract variables.
func engineEnv(environ []string) []string {
	env := make([]string, 0, len(environ)+3)
	for _, kv := range environ {
		if !strings.HasPrefix(kv, envPrefix) {
			env = append(env, kv)
		}
	}
	return append(env, "TF_IN_AUTOMATION=1", "TF_INPUT=0", "CHECKPOINT_DISABLE=1")
}
