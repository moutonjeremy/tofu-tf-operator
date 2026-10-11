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

// Package engine provides the OpenTofu and Terraform binaries used by the runner.
// Downloaded releases are verified against the GPG signature of their checksums,
// with the keys of OpenTofu and HashiCorp embedded in the official libraries.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	goversion "github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
	"github.com/opentofu/tofudl"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// Environment variables configuring release mirrors, e.g. for air-gapped clusters.
// They can be set on the runner through spec.env.
const (
	// EnvTofuAPIURL is the URL of the JSON API listing the OpenTofu releases.
	EnvTofuAPIURL = "TFOP_TOFU_API_URL"
	// EnvTofuMirrorURLTemplate is the template of the OpenTofu artifact URLs (see tofudl).
	EnvTofuMirrorURLTemplate = "TFOP_TOFU_MIRROR_URL_TEMPLATE"
	// EnvTerraformReleasesURL is a mirror of releases.hashicorp.com.
	EnvTerraformReleasesURL = "TFOP_TERRAFORM_RELEASES_URL"
)

// Installer provides engine binaries, downloading and verifying them when needed.
type Installer struct {
	// CacheDir is where downloaded binaries are stored.
	CacheDir string
	// TofuOptions configure the OpenTofu downloader (mirror, GPG key).
	TofuOptions []tofudl.ConfigOpt
	// TerraformReleasesURL is a mirror of releases.hashicorp.com; empty for the official site.
	TerraformReleasesURL string
	// TerraformPublicKey replaces the HashiCorp signing key, for tests.
	TerraformPublicKey string
}

// NewInstaller returns an installer using the official release sites, or the
// mirrors configured in the environment.
func NewInstaller(cacheDir string) *Installer {
	i := &Installer{CacheDir: cacheDir, TerraformReleasesURL: os.Getenv(EnvTerraformReleasesURL)}
	if url := os.Getenv(EnvTofuAPIURL); url != "" {
		i.TofuOptions = append(i.TofuOptions, tofudl.ConfigAPIURL(url))
	}
	if tpl := os.Getenv(EnvTofuMirrorURLTemplate); tpl != "" {
		i.TofuOptions = append(i.TofuOptions, tofudl.ConfigDownloadMirrorURLTemplate(tpl))
	}
	return i
}

// Ensure returns the path of the binary of the engine at the given version.
// The binary found in PATH is used when version is empty or when it matches;
// otherwise the release is downloaded and verified.
func (i *Installer) Ensure(ctx context.Context, engine infrav1alpha1.EngineType, version string) (string, error) {
	name := string(engine)
	if path, err := exec.LookPath(name); err == nil {
		if version == "" {
			return path, nil
		}
		if installed, err := BinaryVersion(ctx, path); err == nil && installed == version {
			return path, nil
		}
	} else if version == "" {
		return "", fmt.Errorf("%s is not installed in the runner image; set spec.engine.version", name)
	}

	dir := filepath.Join(i.CacheDir, name, version)
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); err == nil {
		return dest, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	var err error
	switch engine {
	case infrav1alpha1.EngineTofu:
		err = i.installTofu(ctx, version, dest)
	case infrav1alpha1.EngineTerraform:
		dest, err = i.installTerraform(ctx, version, dir)
	default:
		err = fmt.Errorf("unknown engine %q", engine)
	}
	if err != nil {
		return "", fmt.Errorf("installing %s %s: %w", name, version, err)
	}
	return dest, nil
}

func (i *Installer) installTofu(ctx context.Context, version, dest string) error {
	downloader, err := tofudl.New(i.TofuOptions...)
	if err != nil {
		return err
	}
	// The downloader verifies the checksum of the archive and the signature of the checksums.
	binary, err := downloader.Download(ctx, tofudl.DownloadOptVersion(tofudl.Version(version)))
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(binary); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func (i *Installer) installTerraform(ctx context.Context, version, dir string) (string, error) {
	v, err := goversion.NewVersion(version)
	if err != nil {
		return "", err
	}
	// hc-install verifies the checksum of the archive and the signature of the checksums.
	release := &releases.ExactVersion{
		Product:          product.Terraform,
		Version:          v,
		InstallDir:       dir,
		ApiBaseURL:       i.TerraformReleasesURL,
		ArmoredPublicKey: i.TerraformPublicKey,
	}
	return release.Install(ctx)
}

// BinaryVersion returns the version reported by an engine binary.
func BinaryVersion(ctx context.Context, path string) (string, error) {
	out, err := exec.CommandContext(ctx, path, "version", "-json").Output()
	if err != nil {
		return "", err
	}
	var v struct {
		Version string `json:"terraform_version"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return "", err
	}
	return v.Version, nil
}
