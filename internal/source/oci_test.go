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

package source

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// pushArtifact pushes an artifact made of the given layers and returns its digest.
func pushArtifact(t *testing.T, ref string, layers ...mutate.Addendum) string {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, "application/vnd.oci.empty.v1+json")
	img, err := mutate.Append(img, layers...)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := name.NewTag(ref, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	digest, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest.String()
}

// fileLayer is a file pushed with `oras push`.
func fileLayer(title, content string) mutate.Addendum {
	return mutate.Addendum{
		Layer:       static.NewLayer([]byte(content), "application/vnd.oci.image.layer.v1.tar"),
		Annotations: map[string]string{titleAnnotation: title},
	}
}

// tarGzLayer is an archive pushed with `flux push artifact`.
func tarGzLayer(t *testing.T, files map[string]string) mutate.Addendum {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for file, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: file, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(content))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return mutate.Addendum{Layer: static.NewLayer(buf.Bytes(), "application/vnd.cncf.flux.content.v1.tar+gzip")}
}

func newRegistry(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(registry.New())
	t.Cleanup(server.Close)
	return strings.TrimPrefix(server.URL, "http://")
}

func TestOCIArtifacts(t *testing.T) {
	ctx := context.Background()
	host := newRegistry(t)
	opts := OCIOptions{URL: "oci://" + host + "/acme/vpc", Insecure: true}

	for kind, tc := range map[string]struct {
		layers []mutate.Addendum
		want   map[string]string
	}{
		"oras files": {
			layers: []mutate.Addendum{fileLayer("main.tf", "# main"), fileLayer("variables.tf", "# vars")},
			want:   map[string]string{"main.tf": "# main", "variables.tf": "# vars"},
		},
		"flux archive": {
			layers: []mutate.Addendum{tarGzLayer(t, map[string]string{"./envs/dev/main.tf": "# dev", "modules/net/main.tf": "# net"})},
			want:   map[string]string{"envs/dev/main.tf": "# dev", "modules/net/main.tf": "# net"},
		},
	} {
		t.Run(kind, func(t *testing.T) {
			tag := strings.ReplaceAll(kind, " ", "-")
			digest := pushArtifact(t, host+"/acme/vpc:"+tag, tc.layers...)

			revision, err := ResolveOCI(ctx, opts, tag, "")
			if err != nil {
				t.Fatal(err)
			}
			if revision != OCIRevision(tag, digest) || OCIDigest(revision) != digest {
				t.Fatalf("unexpected revision %q for digest %s", revision, digest)
			}

			dir := t.TempDir()
			if err := PullOCI(ctx, opts, digest, dir); err != nil {
				t.Fatal(err)
			}
			for file, want := range tc.want {
				got, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
				if err != nil || string(got) != want {
					t.Errorf("%s = %q, %v; want %q", file, got, err, want)
				}
			}
		})
	}

	if revision, err := ResolveOCI(ctx, opts, "", "sha256:abc"); err != nil || revision != "sha256:abc" {
		t.Fatalf("a digest is its own revision, got %q, %v", revision, err)
	}
	if _, err := ResolveOCI(ctx, opts, "missing", ""); err == nil {
		t.Fatal("expected an error for a missing tag")
	}
}

func TestOCIRejectsEscapingArchives(t *testing.T) {
	host := newRegistry(t)
	opts := OCIOptions{URL: "oci://" + host + "/acme/evil", Insecure: true}
	digest := pushArtifact(t, host+"/acme/evil:v1", tarGzLayer(t, map[string]string{"../../etc/evil.tf": "x"}))
	err := PullOCI(context.Background(), opts, digest, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "outside of the artifact") {
		t.Fatalf("expected the archive to be rejected, got %v", err)
	}
}

func TestOCICredentials(t *testing.T) {
	host := newRegistry(t)
	// The registry only answers with the right credentials.
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:token"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		r.URL.Host, r.URL.Scheme = host, "http"
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer server.Close()
	private := strings.TrimPrefix(server.URL, "http://")
	digest := pushArtifact(t, host+"/acme/private:v1", fileLayer("main.tf", "# private"))

	opts := OCIOptions{URL: "oci://" + private + "/acme/private", Insecure: true}
	if _, err := ResolveOCI(context.Background(), opts, "v1", ""); err == nil {
		t.Fatal("expected an authentication error without credentials")
	}
	opts.DockerConfig = fmt.Appendf(nil, `{"auths":{%q:{"username":"user","password":"token"}}}`, "http://"+private)
	if _, err := ResolveOCI(context.Background(), opts, "v1", ""); err != nil {
		t.Fatalf("expected the credentials of the docker config to be used, got %v", err)
	}
	dir := t.TempDir()
	if err := PullOCI(context.Background(), opts, digest, dir); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "main.tf")); string(got) != "# private" {
		t.Fatalf("unexpected content %q", got)
	}
}

func TestRegistryHost(t *testing.T) {
	for server, want := range map[string]string{
		"https://index.docker.io/v1/": "index.docker.io",
		"docker.io":                   "index.docker.io",
		"ghcr.io":                     "ghcr.io",
		"https://registry.local:5000": "registry.local:5000",
	} {
		if got := registryHost(server); got != want {
			t.Errorf("registryHost(%q) = %q, want %q", server, got, want)
		}
	}
}
