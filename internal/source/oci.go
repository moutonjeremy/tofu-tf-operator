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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

const (
	// DockerConfigKey is the key of the registry credentials in a kubernetes.io/dockerconfigjson Secret.
	DockerConfigKey = ".dockerconfigjson"

	ociScheme = "oci://"
	// titleAnnotation names the file held by a layer, as set by `oras push`.
	titleAnnotation = "org.opencontainers.image.title"
	// maxArtifactSize bounds what is downloaded and extracted.
	maxArtifactSize = 256 << 20
)

// OCIRevision formats the revision of an artifact resolved from a tag, e.g. "v1.2.0@sha256:4f2c...".
func OCIRevision(tag, digest string) string {
	if tag == "" {
		return digest
	}
	return tag + "@" + digest
}

// OCIDigest extracts the digest from an OCI revision.
func OCIDigest(revision string) string {
	if i := strings.LastIndex(revision, "@"); i >= 0 {
		return revision[i+1:]
	}
	return revision
}

// OCIOptions locates an artifact and the credentials to pull it.
type OCIOptions struct {
	// URL is the repository, e.g. oci://ghcr.io/acme/modules/vpc.
	URL string
	// Insecure allows plain HTTP registries.
	Insecure bool
	// DockerConfig is the content of a .dockerconfigjson file, if any.
	DockerConfig []byte
}

func (o OCIOptions) repository() (name.Repository, error) {
	if !strings.HasPrefix(o.URL, ociScheme) {
		return name.Repository{}, fmt.Errorf("the OCI URL %q must start with %s", o.URL, ociScheme)
	}
	var opts []name.Option
	if o.Insecure {
		opts = append(opts, name.Insecure)
	}
	return name.NewRepository(strings.TrimPrefix(o.URL, ociScheme), opts...)
}

func (o OCIOptions) remoteOptions(ctx context.Context) ([]remote.Option, error) {
	keychain, err := dockerConfigKeychain(o.DockerConfig)
	if err != nil {
		return nil, err
	}
	return []remote.Option{remote.WithContext(ctx), remote.WithAuthFromKeychain(keychain)}, nil
}

// ResolveOCI returns the revision of an artifact: the digest of a tag (latest by
// default) or the given digest, without downloading the artifact.
func ResolveOCI(ctx context.Context, opts OCIOptions, tag, digest string) (string, error) {
	repo, err := opts.repository()
	if err != nil {
		return "", err
	}
	if digest != "" {
		return OCIRevision("", digest), nil
	}
	if tag == "" {
		tag = "latest"
	}
	remoteOpts, err := opts.remoteOptions(ctx)
	if err != nil {
		return "", err
	}
	desc, err := remote.Head(repo.Tag(tag), remoteOpts...)
	if err != nil {
		return "", fmt.Errorf("resolving the tag %s of %s: %w", tag, opts.URL, err)
	}
	return OCIRevision(tag, desc.Digest.String()), nil
}

// PullOCI downloads the artifact with the given digest into dir. Layers in tar+gzip
// format (`flux push artifact`, directories pushed with oras) are extracted; other
// layers are written as the file named by their title annotation (`oras push`).
func PullOCI(ctx context.Context, opts OCIOptions, digest, dir string) error {
	repo, err := opts.repository()
	if err != nil {
		return err
	}
	remoteOpts, err := opts.remoteOptions(ctx)
	if err != nil {
		return err
	}
	// Getting the manifest by digest verifies its content.
	desc, err := remote.Get(repo.Digest(digest), remoteOpts...)
	if err != nil {
		return fmt.Errorf("pulling %s@%s: %w", opts.URL, digest, err)
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(desc.Manifest))
	if err != nil {
		return fmt.Errorf("reading the manifest of %s@%s: %w", opts.URL, digest, err)
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("the artifact %s@%s has no layer", opts.URL, digest)
	}

	var total int64
	for _, layer := range manifest.Layers {
		total += layer.Size
		if layer.Size > maxArtifactSize || total > maxArtifactSize {
			return fmt.Errorf("the artifact %s@%s is larger than %d bytes", opts.URL, digest, maxArtifactSize)
		}
		blob, err := remote.Layer(repo.Digest(layer.Digest.String()), remoteOpts...)
		if err != nil {
			return err
		}
		if err := writeLayer(blob, layer, dir); err != nil {
			return fmt.Errorf("extracting the layer %s: %w", layer.Digest, err)
		}
	}
	return nil
}

// writeLayer writes the content of a layer in dir.
func writeLayer(blob v1.Layer, layer v1.Descriptor, dir string) error {
	rc, err := blob.Compressed()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	content := io.LimitReader(rc, maxArtifactSize)

	title := layer.Annotations[titleAnnotation]
	if strings.HasSuffix(string(layer.MediaType), "tar+gzip") || strings.HasSuffix(string(layer.MediaType), "tar.gzip") {
		target := dir
		// A directory pushed with oras is extracted under its title.
		if title != "" && layer.Annotations["io.deis.oras.content.unpack"] == "true" {
			if !filepath.IsLocal(title) {
				return fmt.Errorf("invalid title %q", title)
			}
			target = filepath.Join(dir, title)
		}
		return extractTarGz(content, target)
	}
	if title == "" {
		return errors.New("the layer is neither a tar+gzip archive nor a titled file")
	}
	if !filepath.IsLocal(title) {
		return fmt.Errorf("invalid title %q", title)
	}
	file := filepath.Join(dir, title)
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(file, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, content); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// extractTarGz extracts the regular files and directories of an archive, refusing
// paths that leave dir and links, which could point outside of it.
func extractTarGz(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		entry := path.Clean(strings.TrimPrefix(header.Name, "./"))
		if entry == "." {
			continue
		}
		if !filepath.IsLocal(entry) {
			return fmt.Errorf("the archive holds the path %q outside of the artifact", header.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(entry))
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, maxArtifactSize)); err != nil {
				_ = out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("the archive holds %q, which is not a regular file nor a directory", header.Name)
		}
	}
}

// dockerConfig is the format of a .dockerconfigjson file.
type dockerConfig struct {
	Auths map[string]authn.AuthConfig `json:"auths"`
}

// dockerConfigKeychain returns the credentials of a .dockerconfigjson file, by registry.
func dockerConfigKeychain(data []byte) (authn.Keychain, error) {
	if len(data) == 0 {
		return authn.NewMultiKeychain(), nil
	}
	var config dockerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("reading the registry credentials: %w", err)
	}
	auths := make(map[string]authn.AuthConfig, len(config.Auths))
	for server, auth := range config.Auths {
		auths[registryHost(server)] = auth
	}
	return keychain(auths), nil
}

type keychain map[string]authn.AuthConfig

func (k keychain) Resolve(resource authn.Resource) (authn.Authenticator, error) {
	if auth, ok := k[registryHost(resource.RegistryStr())]; ok {
		return authn.FromConfig(auth), nil
	}
	return authn.Anonymous, nil
}

// registryHost normalizes the registry keys of a docker config ("https://index.docker.io/v1/", "ghcr.io").
func registryHost(server string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://")
	host, _, _ = strings.Cut(host, "/")
	if host == "docker.io" || host == "registry-1.docker.io" {
		return name.DefaultRegistry
	}
	return host
}
