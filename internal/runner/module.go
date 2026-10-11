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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// backendOverrideFile is the name of the generated backend override, without extension.
const backendOverrideFile = "zz_tofu_operator_backend_override"

// writeBackendOverride forces the backend of the root module in dir.
func writeBackendOverride(dir string, cfg Config) error {
	switch cfg.BackendType {
	case infrav1alpha1.BackendSource:
		return nil
	case infrav1alpha1.BackendCustom:
		return os.WriteFile(filepath.Join(dir, backendOverrideFile+".tf"), []byte(cfg.BackendCustom), 0o644)
	case infrav1alpha1.BackendKubernetes:
		override := map[string]any{
			"terraform": map[string]any{
				"backend": map[string]any{
					"kubernetes": map[string]any{
						"secret_suffix":     cfg.WorkspaceName,
						"namespace":         cfg.Namespace,
						"in_cluster_config": true,
					},
				},
			},
		}
		content, err := json.MarshalIndent(override, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, backendOverrideFile+".tf.json"), content, 0o644)
	}
	return fmt.Errorf("unknown backend type %q", cfg.BackendType)
}

var variableSchema = &hcl.BodySchema{
	Blocks: []hcl.BlockHeaderSchema{{Type: "variable", LabelNames: []string{"name"}}},
}

// declaredVariables returns the names of the variables declared by the root module in dir.
func declaredVariables(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	parser := hclparse.NewParser()
	declared := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var file *hcl.File
		var diags hcl.Diagnostics
		switch name := entry.Name(); {
		case strings.HasSuffix(name, ".tf"), strings.HasSuffix(name, ".tofu"):
			file, diags = parser.ParseHCLFile(path)
		case strings.HasSuffix(name, ".tf.json"), strings.HasSuffix(name, ".tofu.json"):
			file, diags = parser.ParseJSONFile(path)
		default:
			continue
		}
		if diags.HasErrors() {
			return nil, diags
		}
		content, _, _ := file.Body.PartialContent(variableSchema)
		for _, block := range content.Blocks {
			declared[block.Labels[0]] = true
		}
	}
	return declared, nil
}

// selectVariables keeps the variables declared by the root module, since passing
// an undeclared variable with -var is an error. It returns the ignored names.
func selectVariables(vars map[string]string, declared map[string]bool) (map[string]string, []string) {
	selected := map[string]string{}
	var ignored []string
	for name, value := range vars {
		if declared[name] {
			selected[name] = value
		} else {
			ignored = append(ignored, name)
		}
	}
	return selected, ignored
}
