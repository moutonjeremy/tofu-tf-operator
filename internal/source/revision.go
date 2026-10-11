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

// Package source resolves and identifies the revisions of Workspace sources.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
)

const sha1Marker = "sha1:"

// InlineRevision identifies the content of an inline source.
func InlineRevision(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// FilesRevision identifies a set of files, such as the keys of a ConfigMap.
// It only depends on the names and contents of the files.
func FilesRevision(files map[string][]byte) string {
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(files)) {
		// Lengths prefix the fields so that no two sets of files share an encoding.
		_, _ = h.Write([]byte{byte(len(name) >> 8), byte(len(name))})
		_, _ = h.Write([]byte(name))
		n := len(files[name])
		_, _ = h.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		_, _ = h.Write(files[name])
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// GitRevision formats the revision of a commit resolved from a ref, e.g. "main@sha1:4f2c...".
func GitRevision(ref, commit string) string {
	if ref == "" {
		return sha1Marker + commit
	}
	return ref + "@" + sha1Marker + commit
}

// GitCommit extracts the commit SHA from a git revision.
func GitCommit(revision string) string {
	if i := strings.LastIndex(revision, sha1Marker); i >= 0 {
		return revision[i+len(sha1Marker):]
	}
	return revision
}
