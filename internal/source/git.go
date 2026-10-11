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
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/storage/memory"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// Keys of the git credentials Secret, shared with the runner.
const (
	GitUsernameKey   = "username"
	GitPasswordKey   = "password"
	GitIdentityKey   = "identity"
	GitKnownHostsKey = "known_hosts"
)

// ResolveGit returns the revision a ref points to on the remote, without cloning.
// secret holds the data of the credentials Secret, if any.
func ResolveGit(ctx context.Context, url string, ref infrav1alpha1.GitRef, secret map[string][]byte) (string, error) {
	if ref.Commit != "" {
		return GitRevision("", ref.Commit), nil
	}

	auth, cleanup, err := gitAuth(url, secret)
	if err != nil {
		return "", err
	}
	defer cleanup()

	remote := git.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{url}})
	refs, err := remote.ListContext(ctx, &git.ListOptions{Auth: auth, PeelingOption: git.AppendPeeled})
	if err != nil {
		return "", fmt.Errorf("listing the refs of %s: %w", url, err)
	}

	name, label := plumbing.HEAD, "HEAD"
	switch {
	case ref.Branch != "":
		name, label = plumbing.NewBranchReferenceName(ref.Branch), ref.Branch
	case ref.Tag != "":
		name, label = plumbing.NewTagReferenceName(ref.Tag), ref.Tag
	}

	byName := make(map[plumbing.ReferenceName]*plumbing.Reference, len(refs))
	for _, r := range refs {
		byName[r.Name()] = r
	}
	// An annotated tag points to a tag object; use the commit it peels to.
	if peeled, ok := byName[name+"^{}"]; ok {
		return GitRevision(label, peeled.Hash().String()), nil
	}
	found, ok := byName[name]
	if ok && found.Type() == plumbing.SymbolicReference {
		found, ok = byName[found.Target()]
	}
	if !ok {
		return "", fmt.Errorf("ref %s not found in %s", label, url)
	}
	return GitRevision(label, found.Hash().String()), nil
}

// gitAuth returns the authentication method matching the credentials and the URL.
func gitAuth(url string, secret map[string][]byte) (transport.AuthMethod, func(), error) {
	noop := func() {}
	if password, ok := secret[GitPasswordKey]; ok {
		username := strings.TrimSpace(string(secret[GitUsernameKey]))
		if username == "" {
			username = "git"
		}
		return &githttp.BasicAuth{Username: username, Password: strings.TrimSpace(string(password))}, noop, nil
	}

	identity, ok := secret[GitIdentityKey]
	if !ok {
		return nil, noop, nil
	}
	knownHosts, ok := secret[GitKnownHostsKey]
	if !ok {
		return nil, noop, fmt.Errorf("an SSH identity requires the %q key in the git Secret", GitKnownHostsKey)
	}
	user := "git"
	if endpoint, err := transport.NewEndpoint(url); err == nil && endpoint.User != "" {
		user = endpoint.User
	}
	keys, err := gitssh.NewPublicKeys(user, identity, "")
	if err != nil {
		return nil, noop, fmt.Errorf("parsing the SSH identity: %w", err)
	}
	// The known hosts callback only reads files.
	file, err := os.CreateTemp("", "known_hosts-*")
	if err != nil {
		return nil, noop, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	_, err = file.Write(knownHosts)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		cleanup()
		return nil, noop, err
	}
	if keys.HostKeyCallback, err = gitssh.NewKnownHostsCallback(file.Name()); err != nil {
		cleanup()
		return nil, noop, fmt.Errorf("parsing known_hosts: %w", err)
	}
	return keys, cleanup, nil
}
