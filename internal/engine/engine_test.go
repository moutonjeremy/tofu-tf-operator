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

package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/opentofu/tofudl"
	"github.com/opentofu/tofudl/mockmirror"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// fakeTofu prints its version like `tofu version -json`. The mock mirror serves version 1.0.0.
const fakeTofu = "#!/bin/sh\necho '{\"terraform_version\":\"1.0.0\"}'\n"

func mirrorOptions(m mockmirror.Mirror, key string) []tofudl.ConfigOpt {
	return []tofudl.ConfigOpt{
		tofudl.ConfigAPIURL(m.APIURL()),
		tofudl.ConfigDownloadMirrorURLTemplate(m.DownloadMirrorURLTemplate()),
		tofudl.ConfigGPGKey(key),
	}
}

func TestEnsureDownloadsSignedTofu(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake binary is a shell script")
	}
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	mirror := mockmirror.NewFromBinary(t, []byte(fakeTofu))
	installer := &Installer{CacheDir: t.TempDir(), TofuOptions: mirrorOptions(mirror, mirror.GPGKey())}

	path, err := installer.Ensure(ctx, infrav1alpha1.EngineTofu, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := BinaryVersion(ctx, path); err != nil || v != "1.0.0" {
		t.Fatalf("installed binary reports %q, %v", v, err)
	}

	// The second call uses the cache, even when the mirror is unreachable.
	installer.TofuOptions = []tofudl.ConfigOpt{tofudl.ConfigAPIURL("http://127.0.0.1:1/api.json")}
	if again, err := installer.Ensure(ctx, infrav1alpha1.EngineTofu, "1.0.0"); err != nil || again != path {
		t.Fatalf("expected the cached binary, got %q, %v", again, err)
	}
}

func TestEnsureRejectsUnsignedTofu(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	mirror := mockmirror.NewFromBinary(t, []byte(fakeTofu))
	// The release is verified with the key of another mirror, as if it had been tampered with.
	other := mockmirror.NewFromBinary(t, []byte(fakeTofu))
	installer := &Installer{CacheDir: t.TempDir(), TofuOptions: mirrorOptions(mirror, other.GPGKey())}

	if _, err := installer.Ensure(context.Background(), infrav1alpha1.EngineTofu, "1.0.0"); err == nil {
		t.Fatal("a release signed with an unknown key must be rejected")
	}
	if _, err := os.Stat(filepath.Join(installer.CacheDir, "tofu", "1.0.0", "tofu")); !os.IsNotExist(err) {
		t.Fatalf("nothing should be installed, got %v", err)
	}
}

func TestEnsureUsesBundledBinary(t *testing.T) {
	bin := os.Getenv("TOFU_BIN")
	if bin == "" {
		t.Skip("TOFU_BIN is not set")
	}
	dir := t.TempDir()
	if err := os.Symlink(bin, filepath.Join(dir, "tofu")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx := context.Background()
	installer := &Installer{CacheDir: t.TempDir()}

	path, err := installer.Ensure(ctx, infrav1alpha1.EngineTofu, "")
	if err != nil || path != filepath.Join(dir, "tofu") {
		t.Fatalf("expected the bundled binary, got %q, %v", path, err)
	}
	version, err := BinaryVersion(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := installer.Ensure(ctx, infrav1alpha1.EngineTofu, version); err != nil || path != filepath.Join(dir, "tofu") {
		t.Fatalf("expected the bundled binary for its own version, got %q, %v", path, err)
	}

	if _, err := installer.Ensure(ctx, infrav1alpha1.EngineTerraform, ""); err == nil {
		t.Fatal("terraform is not bundled: a version is required")
	}
}

// TestEnsureDownloadsOfficialReleases downloads real releases, verified with the
// embedded keys of OpenTofu and HashiCorp; it needs network access.
func TestEnsureDownloadsOfficialReleases(t *testing.T) {
	if os.Getenv("ENGINE_NETWORK_TESTS") == "" {
		t.Skip("set ENGINE_NETWORK_TESTS=1 to download official releases")
	}
	t.Setenv("PATH", t.TempDir())
	ctx := context.Background()
	installer := NewInstaller(t.TempDir())
	for engine, version := range map[infrav1alpha1.EngineType]string{
		infrav1alpha1.EngineTofu:      "1.12.7",
		infrav1alpha1.EngineTerraform: "1.5.7",
	} {
		path, err := installer.Ensure(ctx, engine, version)
		if err != nil {
			t.Fatal(err)
		}
		if v, err := BinaryVersion(ctx, path); err != nil || v != version {
			t.Fatalf("installed %s reports %q, %v", engine, v, err)
		}
	}
}
