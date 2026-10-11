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
	"fmt"
	"os"
	"os/exec"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/labbs/tofu-tf-operator/test/utils"
)

var (
	// The images are loaded in Kind, so their tags must not be "latest", which is always pulled.
	managerImage = "example.com/tofu-tf-operator:v0.0.1"
	runnerImage  = "example.com/tofu-runner:v0.0.1"
)

// TestE2E runs the e2e test suite to validate the solution in an isolated environment.
// The default setup requires Kind; set CONTAINER_TOOL=podman to build the images with podman.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting tofu-tf-operator e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("building the runner image")
	cmd = exec.Command("make", "docker-build-runner", fmt.Sprintf("RUNNER_IMG=%s", runnerImage))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the runner image")

	By("loading the images on Kind")
	for _, image := range []string{managerImage, runnerImage} {
		err = utils.LoadImageToKindClusterWithName(image)
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load %s into Kind", image)
	}

	configureKubectlKubeRC()
})

// configureKubectlKubeRC disables kubectl kuberc unless KUBECTL_KUBERC=true.
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}
