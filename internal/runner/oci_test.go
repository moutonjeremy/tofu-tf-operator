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
	"io"
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

	"github.com/labbs/tofu-tf-operator/internal/source"
)

func TestFetchOCI(t *testing.T) {
	server := httptest.NewServer(registry.New())
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "http://")

	// An artifact pushed with `oras push ... envs/dev/main.tf`.
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img, err := mutate.Append(img, mutate.Addendum{
		Layer:       static.NewLayer([]byte("# dev\n"), "application/vnd.oci.image.layer.v1.tar"),
		Annotations: map[string]string{"org.opencontainers.image.title": "envs/dev/main.tf"},
	})
	if err != nil {
		t.Fatal(err)
	}
	tag, _ := name.NewTag(host+"/acme/vpc:v1", name.Insecure)
	if err := remote.Write(tag, img); err != nil {
		t.Fatal(err)
	}
	digest, _ := img.Digest()

	r := &Runner{
		Config: Config{
			SourceKind:        SourceOCI,
			OCIURL:            "oci://" + host + "/acme/vpc",
			OCIPath:           "envs/dev",
			OCIInsecure:       true,
			OCICredentialsDir: t.TempDir(),
			Revision:          source.OCIRevision("v1", digest.String()),
			WorkDir:           t.TempDir(),
		},
		Log: io.Discard,
	}
	dir, revision, err := r.fetchSource(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "main.tf"))
	if err != nil || string(content) != "# dev\n" || revision != r.Config.Revision {
		t.Fatalf("unexpected fetch: %q, %v at %s", content, err, revision)
	}

	r.Config.OCIPath = "../escape"
	if _, _, err := r.fetchSource(context.Background()); err == nil {
		t.Fatal("a path outside of the artifact must be rejected")
	}
}
