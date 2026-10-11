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
	"encoding/json"
	"strings"
	"sync"
	"unicode/utf8"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

// MaxResultSize is the size limit of a container termination message.
const MaxResultSize = 4096

// Result is the outcome of a step, written to the termination message of the
// runner container and read back by the Run controller.
type Result struct {
	Step Step `json:"step"`
	// Revision is the source revision actually used.
	Revision string `json:"revision,omitempty"`
	// Plan is set by a successful plan step.
	Plan *infrav1alpha1.PlanSummary `json:"plan,omitempty"`
	// Error is set when the step failed.
	Error string `json:"error,omitempty"`
	// StalePlan is set when the state changed since the plan was made.
	StalePlan bool `json:"stalePlan,omitempty"`
	// LogTail holds the last lines of the output when the step failed.
	LogTail string `json:"logTail,omitempty"`
}

// Encode returns the JSON representation of the result, shortening the log tail
// and then the error so that it fits in a termination message.
func (r Result) Encode() []byte {
	for {
		data, err := json.Marshal(r)
		if err != nil {
			return []byte(`{"error":"encoding result"}`)
		}
		excess := len(data) - MaxResultSize
		switch {
		case excess <= 0:
			return data
		case r.LogTail != "":
			r.LogTail = dropPrefix(r.LogTail, excess)
		case len(r.Error) <= 3:
			return data
		default:
			r.Error = r.Error[:max(0, len(r.Error)-excess-3)] + "..."
		}
	}
}

// DecodeResult parses a termination message. It returns false when the message
// was not written by the runner, e.g. the log tail used by the kubelet on a crash.
func DecodeResult(message string) (Result, bool) {
	var r Result
	if err := json.Unmarshal([]byte(message), &r); err != nil || r.Step == "" {
		return Result{}, false
	}
	return r, true
}

// dropPrefix removes at least n bytes from the start of s, up to the next line when possible.
func dropPrefix(s string, n int) string {
	if n >= len(s) {
		return ""
	}
	s = s[n:]
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
		return s[i+1:]
	}
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// tailBuffer keeps the last bytes written to it.
type tailBuffer struct {
	mu        sync.Mutex
	size      int
	buf       []byte
	truncated bool
}

func newTailBuffer(size int) *tailBuffer {
	return &tailBuffer{size: size}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.size {
		t.buf = append(t.buf[:0], t.buf[len(t.buf)-t.size:]...)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.truncated {
		return string(t.buf)
	}
	// Drop the first line, which is likely partial.
	return dropPrefix(string(t.buf), 0)
}
