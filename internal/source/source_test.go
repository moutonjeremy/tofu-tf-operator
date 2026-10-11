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
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

func TestFilesRevision(t *testing.T) {
	a := FilesRevision(map[string][]byte{"main.tf": []byte("a"), "vars.tf": []byte("b")})
	b := FilesRevision(map[string][]byte{"vars.tf": []byte("b"), "main.tf": []byte("a")})
	if a != b {
		t.Fatal("the revision must not depend on the iteration order")
	}
	// Moving bytes between names or contents must change the revision.
	if FilesRevision(map[string][]byte{"ab": []byte("c")}) == FilesRevision(map[string][]byte{"a": []byte("bc")}) {
		t.Fatal("different files share a revision")
	}
	if !strings.HasPrefix(a, "sha256:") {
		t.Fatalf("unexpected revision %q", a)
	}
}

func TestGitRevision(t *testing.T) {
	const sha = "4f2c0d2c5c8a4e0f6c1c2a3b4d5e6f708192a3b4"
	for _, ref := range []string{"main", "feature/x", "v1.0.0", ""} {
		if got := GitCommit(GitRevision(ref, sha)); got != sha {
			t.Fatalf("GitCommit(GitRevision(%q)) = %q", ref, got)
		}
	}
	if got := GitRevision("main", sha); got != "main@sha1:"+sha {
		t.Fatalf("unexpected revision %q", got)
	}
}

func TestResolveGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(content string) string {
		if err := os.WriteFile(filepath.Join(repo, "main.tf"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", ".")
		git("commit", "--quiet", "-m", content)
		return git("rev-parse", "HEAD")
	}

	git("init", "--quiet", "--initial-branch=main")
	first := commit("v1")
	git("tag", "light")
	git("tag", "-a", "annotated", "-m", "annotated tag")
	git("checkout", "--quiet", "-b", "feature")
	feature := commit("v2")
	git("checkout", "--quiet", "main")

	url := "file://" + repo
	ctx := context.Background()
	for name, tc := range map[string]struct {
		ref  infrav1alpha1.GitRef
		want string
	}{
		"head":            {infrav1alpha1.GitRef{}, GitRevision("HEAD", first)},
		"branch":          {infrav1alpha1.GitRef{Branch: "feature"}, GitRevision("feature", feature)},
		"lightweight tag": {infrav1alpha1.GitRef{Tag: "light"}, GitRevision("light", first)},
		"annotated tag":   {infrav1alpha1.GitRef{Tag: "annotated"}, GitRevision("annotated", first)},
		"commit":          {infrav1alpha1.GitRef{Commit: feature}, GitRevision("", feature)},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ResolveGit(ctx, url, tc.ref, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}

	if _, err := ResolveGit(ctx, url, infrav1alpha1.GitRef{Branch: "missing"}, nil); err == nil {
		t.Fatal("expected an error for a missing branch")
	}
	if _, err := ResolveGit(ctx, url, infrav1alpha1.GitRef{}, map[string][]byte{GitIdentityKey: []byte("key")}); err == nil {
		t.Fatal("an SSH identity without known_hosts must be rejected")
	}
}
