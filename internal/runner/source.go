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
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/labbs/tofu-tf-operator/internal/source"
)

// fetchSource writes the configuration under the work directory. It returns the
// root module directory and the revision actually fetched.
func (r *Runner) fetchSource(ctx context.Context) (string, string, error) {
	switch r.Config.SourceKind {
	case SourceInline:
		dir := filepath.Join(r.Config.WorkDir, "src")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(r.Config.InlineSource), 0o644); err != nil {
			return "", "", err
		}
		return dir, source.InlineRevision(r.Config.InlineSource), nil

	case SourceConfigMap:
		dir := filepath.Join(r.Config.WorkDir, "src")
		files, err := readMountedFiles(r.Config.ConfigMapDir)
		if err != nil {
			return "", "", err
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", "", err
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
				return "", "", err
			}
		}
		return dir, source.FilesRevision(files), nil

	case SourceGit:
		return r.fetchGit(ctx)

	case SourceOCI:
		return r.fetchOCI(ctx)
	}
	return "", "", fmt.Errorf("unknown source kind %q", r.Config.SourceKind)
}

// readMountedFiles reads the files of a mounted ConfigMap, skipping the
// hidden entries managed by the kubelet (..data, ..2026_...).
func readMountedFiles(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading ConfigMap source: %w", err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "..") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		files[entry.Name()] = content
	}
	return files, nil
}

// fetchGit checks out the commit of the revision.
func (r *Runner) fetchGit(ctx context.Context) (string, string, error) {
	commit := source.GitCommit(r.Config.Revision)
	if commit == "" {
		return "", "", errors.New("git source without revision")
	}
	if r.Config.GitPath != "" && !filepath.IsLocal(r.Config.GitPath) {
		return "", "", fmt.Errorf("git path %q must be relative to the repository", r.Config.GitPath)
	}
	if err := r.configureGit(); err != nil {
		return "", "", err
	}

	repo := filepath.Join(r.Config.WorkDir, "repo")
	git := func(args ...string) error {
		return runCommand(ctx, r.Log, repo, r.env, "git", args...)
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		return "", "", err
	}
	if err := git("init", "--quiet"); err != nil {
		return "", "", err
	}
	if err := git("remote", "add", "origin", r.Config.GitURL); err != nil {
		return "", "", err
	}
	// Most servers allow fetching a commit directly; fall back to fetching
	// every branch and tag otherwise.
	if err := git("fetch", "--quiet", "--depth=1", "origin", commit); err != nil {
		_, _ = fmt.Fprintln(r.Log, "fetching the commit directly failed, fetching all refs")
		if err := git("fetch", "--quiet", "--tags", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return "", "", err
		}
	}
	if err := git("-c", "advice.detachedHead=false", "checkout", "--quiet", commit); err != nil {
		return "", "", err
	}

	dir := filepath.Join(repo, filepath.FromSlash(r.Config.GitPath))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("path %q not found in the repository", r.Config.GitPath)
	}
	return dir, r.Config.Revision, nil
}

// fetchOCI pulls the artifact of the revision.
func (r *Runner) fetchOCI(ctx context.Context) (string, string, error) {
	if r.Config.OCIPath != "" && !filepath.IsLocal(r.Config.OCIPath) {
		return "", "", fmt.Errorf("OCI path %q must be relative to the artifact", r.Config.OCIPath)
	}
	creds, err := readOptionalFiles(r.Config.OCICredentialsDir, source.DockerConfigKey)
	if err != nil {
		return "", "", err
	}
	opts := source.OCIOptions{URL: r.Config.OCIURL, Insecure: r.Config.OCIInsecure, DockerConfig: creds[source.DockerConfigKey]}
	digest := source.OCIDigest(r.Config.Revision)
	_, _ = fmt.Fprintf(r.Log, "pulling %s@%s\n", r.Config.OCIURL, digest)

	artifact := filepath.Join(r.Config.WorkDir, "artifact")
	if err := os.MkdirAll(artifact, 0o755); err != nil {
		return "", "", err
	}
	if err := source.PullOCI(ctx, opts, digest, artifact); err != nil {
		return "", "", err
	}
	dir := filepath.Join(artifact, filepath.FromSlash(r.Config.OCIPath))
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("path %q not found in the artifact", r.Config.OCIPath)
	}
	return dir, r.Config.Revision, nil
}

// configureGit writes the git configuration of the runner's HOME so that the
// credentials are used both for the checkout and for the modules fetched by the engine.
func (r *Runner) configureGit() error {
	creds, err := readOptionalFiles(r.Config.GitCredentialsDir, source.GitUsernameKey, source.GitPasswordKey, source.GitIdentityKey, source.GitKnownHostsKey)
	if err != nil {
		return err
	}

	var gitconfig strings.Builder
	if password, ok := creds[source.GitPasswordKey]; ok {
		u, err := url.Parse(r.Config.GitURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("username/password credentials require an http(s) URL, got %q", r.Config.GitURL)
		}
		username := strings.TrimSpace(string(creds[source.GitUsernameKey]))
		if username == "" {
			username = "git"
		}
		userinfo := url.UserPassword(username, strings.TrimSpace(string(password)))
		store := filepath.Join(r.home, ".git-credentials")
		line := fmt.Sprintf("%s://%s@%s\n", u.Scheme, userinfo.String(), u.Host)
		if err := os.WriteFile(store, []byte(line), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(&gitconfig, "[credential]\n\thelper = store --file=%s\n", store)
	}
	if identity, ok := creds[source.GitIdentityKey]; ok {
		knownHosts, ok := creds[source.GitKnownHostsKey]
		if !ok {
			return fmt.Errorf("an SSH identity requires the %q key in the git Secret", source.GitKnownHostsKey)
		}
		sshDir := filepath.Join(r.home, ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			return err
		}
		identityFile := filepath.Join(sshDir, "identity")
		knownHostsFile := filepath.Join(sshDir, "known_hosts")
		if err := os.WriteFile(identityFile, identity, 0o600); err != nil {
			return err
		}
		if err := os.WriteFile(knownHostsFile, knownHosts, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(&gitconfig, "[core]\n\tsshCommand = ssh -i %s -o IdentitiesOnly=yes -o UserKnownHostsFile=%s -o StrictHostKeyChecking=yes\n",
			identityFile, knownHostsFile)
	}
	return os.WriteFile(filepath.Join(r.home, ".gitconfig"), []byte(gitconfig.String()), 0o600)
}

// readOptionalFiles reads the given files of a directory, ignoring the missing ones.
func readOptionalFiles(dir string, names ...string) (map[string][]byte, error) {
	files := map[string][]byte{}
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			return nil, err
		}
		files[name] = content
	}
	return files, nil
}

// runCommand runs a command, sending its output to out.
func runCommand(ctx context.Context, out io.Writer, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, args[0], err)
	}
	return nil
}
