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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// jsonPlan is the subset of the `show -json` plan representation used by the runner.
type jsonPlan struct {
	ResourceChanges []struct {
		Change struct {
			Actions   []string        `json:"actions"`
			Importing json.RawMessage `json:"importing,omitempty"`
		} `json:"change"`
	} `json:"resource_changes"`
}

// Summarize counts the resource changes of a plan the way the CLI does:
// a replacement counts as one creation and one destruction.
func Summarize(planJSON []byte) (infrav1alpha1.PlanSummary, error) {
	var plan jsonPlan
	if err := json.Unmarshal(planJSON, &plan); err != nil {
		return infrav1alpha1.PlanSummary{}, fmt.Errorf("decoding plan: %w", err)
	}

	var summary infrav1alpha1.PlanSummary
	for _, rc := range plan.ResourceChanges {
		if len(rc.Change.Importing) > 0 && string(rc.Change.Importing) != "null" {
			summary.Import++
		}
		for _, action := range rc.Change.Actions {
			switch action {
			case "create":
				summary.Add++
			case "update":
				summary.Change++
			case "delete":
				summary.Destroy++
			}
		}
	}
	return summary, nil
}

// Digest is the identifier of a plan file.
func Digest(plan []byte) string {
	sum := sha256.Sum256(plan)
	return "sha256:" + hex.EncodeToString(sum[:])
}
