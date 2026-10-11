//go:build e2e
// +build e2e

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

package e2e

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/labbs/tofu-tf-operator/test/utils"
)

// workloadNamespace holds the Workspaces of the tests. It enforces the restricted
// Pod Security Standard, which the runner pods must satisfy.
const workloadNamespace = "e2e-workloads"

func kubectl(args ...string) (string, error) {
	return utils.Run(exec.Command("kubectl", args...))
}

func kubectlApply(manifest string) {
	GinkgoHelper()
	cmd := exec.Command("kubectl", "-n", workloadNamespace, "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())
}

func jsonPath(kind, name, path string) func() (string, error) {
	return func() (string, error) {
		return kubectl("-n", workloadNamespace, "get", kind, name, "-o", "jsonpath="+path)
	}
}

// stateResources returns the number of resources in the state stored by the Kubernetes backend.
func stateResources(workspace string) (int, error) {
	encoded, err := kubectl("-n", workloadNamespace, "get", "secret", "tfstate-default-"+workspace, "-o", "jsonpath={.data.tfstate}")
	if err != nil {
		return 0, err
	}
	compressed, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return 0, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return 0, err
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		return 0, err
	}
	var state struct {
		Resources []json.RawMessage `json:"resources"`
	}
	err = json.Unmarshal(raw, &state)
	return len(state.Resources), err
}

// workspaceSpecs runs Workspaces on the deployed operator. They only use the
// built-in terraform_data resource, so no provider is downloaded.
func workspaceSpecs() {
	Context("Workspace", func() {
		BeforeAll(func() {
			_, err := kubectl("create", "ns", workloadNamespace)
			Expect(err).NotTo(HaveOccurred())
			_, err = kubectl("label", "--overwrite", "ns", workloadNamespace, "pod-security.kubernetes.io/enforce=restricted")
			Expect(err).NotTo(HaveOccurred())
		})

		AfterAll(func() {
			// Release the Workspaces while the operator still runs, so the namespace can be deleted.
			_, _ = kubectl("-n", workloadNamespace, "delete", "workspaces", "--all", "--timeout=3m")
			_, _ = kubectl("delete", "ns", workloadNamespace, "--wait=false")
		})

		AfterEach(func() {
			if !CurrentSpecReport().Failed() {
				return
			}
			out, _ := kubectl("-n", workloadNamespace, "get", "workspaces,runs,jobs,pods,secrets", "-o", "wide")
			_, _ = fmt.Fprintf(GinkgoWriter, "Workload objects:\n%s\n", out)
			out, _ = kubectl("-n", workloadNamespace, "get", "runs", "-o", "yaml")
			_, _ = fmt.Fprintf(GinkgoWriter, "Runs:\n%s\n", out)
			out, _ = kubectl("-n", workloadNamespace, "logs", "-l", "app.kubernetes.io/name=tofu-runner", "--tail=50", "--prefix")
			_, _ = fmt.Fprintf(GinkgoWriter, "Runner logs:\n%s\n", out)
		})

		It("plans, applies and exports the outputs", func() {
			kubectlApply(`
apiVersion: infra.labbs.fr/v1alpha1
kind: Workspace
metadata:
  name: auto
spec:
  source:
    inline: |
      variable "greeting" {
        type = string
      }
      resource "terraform_data" "this" {
        input = var.greeting
      }
      output "message" {
        value = "${terraform_data.this.output} from tofu"
      }
  vars:
    - name: greeting
      value: hello
  outputs:
    secretName: auto-outputs
  destroyOnDelete: true
`)
			Eventually(jsonPath("run", "auto-1", "{.status.phase}"), 3*time.Minute).Should(Equal("Applied"))
			Eventually(jsonPath("workspace", "auto", `{.status.conditions[?(@.type=="Ready")].status}`)).Should(Equal("True"))

			encoded, err := jsonPath("secret", "auto-outputs", "{.data.message}")()
			Expect(err).NotTo(HaveOccurred())
			message, err := base64.StdEncoding.DecodeString(encoded)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(message)).To(Equal("hello from tofu"))
			Expect(stateResources("auto")).To(Equal(1))
		})

		It("waits for the approval of the plan", func() {
			kubectlApply(`
apiVersion: infra.labbs.fr/v1alpha1
kind: Workspace
metadata:
  name: manual
spec:
  approvalMode: Manual
  source:
    inline: |
      resource "terraform_data" "this" {
        input = "v1"
      }
`)
			Eventually(jsonPath("run", "manual-1", "{.status.phase}"), 3*time.Minute).Should(Equal("AwaitingApproval"))
			Expect(jsonPath("run", "manual-1", "{.status.plan.add}")()).To(Equal("1"))
			Consistently(jsonPath("run", "manual-1", "{.status.phase}"), 5*time.Second).Should(Equal("AwaitingApproval"))

			By("reading the plan and approving it with the kubectl plugin")
			_, err := utils.Run(exec.Command("make", "build-plugin"))
			Expect(err).NotTo(HaveOccurred())
			out, err := utils.Run(exec.Command("bin/kubectl-tofu", "-n", workloadNamespace, "logs", "manual", "--step", "plan"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Plan: 1 to add, 0 to change, 0 to destroy."))
			out, err = utils.Run(exec.Command("bin/kubectl-tofu", "-n", workloadNamespace, "approve", "manual", "--yes"))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("run manual-1 approved"))
			Eventually(jsonPath("run", "manual-1", "{.status.phase}"), 3*time.Minute).Should(Equal("Applied"))
		})

		It("reports the error of an invalid configuration", func() {
			kubectlApply(`
apiVersion: infra.labbs.fr/v1alpha1
kind: Workspace
metadata:
  name: invalid
spec:
  source:
    inline: |
      resource "terraform_data" "this" {
        unknown_argument = true
      }
`)
			Eventually(jsonPath("run", "invalid-1", "{.status.phase}"), 3*time.Minute).Should(Equal("Failed"))
			Eventually(jsonPath("workspace", "invalid", `{.status.conditions[?(@.type=="Ready")].message}`)).
				Should(ContainSubstring("Unsupported argument"))
		})

		It("destroys the resources when the Workspace is deleted", func() {
			_, err := kubectl("-n", workloadNamespace, "delete", "workspace", "auto", "--wait=false")
			Expect(err).NotTo(HaveOccurred())
			Eventually(func() error {
				_, err := kubectl("-n", workloadNamespace, "get", "workspace", "auto")
				return err
			}, 3*time.Minute).Should(HaveOccurred(), "the Workspace should be deleted")
			Expect(stateResources("auto")).To(Equal(0))
		})
	})
}
