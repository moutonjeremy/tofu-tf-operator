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

package receiver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/controller"
)

const (
	namespace = "infra"
	token     = "s3cr3t"
)

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// newServer returns a server knowing one Receiver of each type, all selecting the
// Workspaces labeled team=platform.
func newServer(t *testing.T) (*Server, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, infrav1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objects := make([]client.Object, 0, 7)
	objects = append(objects,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "hook-token", Namespace: namespace},
			Data:       map[string][]byte{infrav1alpha1.ReceiverTokenKey: []byte(token + "\n")},
		},
		workspace("vpc", "platform"),
		workspace("dns", "platform"),
		workspace("other", "data"),
	)
	for _, kind := range []infrav1alpha1.ReceiverType{infrav1alpha1.ReceiverGitHub, infrav1alpha1.ReceiverGitLab, infrav1alpha1.ReceiverGeneric} {
		objects = append(objects, &infrav1alpha1.Receiver{
			ObjectMeta: metav1.ObjectMeta{Name: string(kind), Namespace: namespace},
			Spec: infrav1alpha1.ReceiverSpec{
				Type:              kind,
				SecretRef:         corev1.LocalObjectReference{Name: "hook-token"},
				WorkspaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "platform"}},
			},
			Status: infrav1alpha1.ReceiverStatus{WebhookPath: controller.ReceiverWebhookPath(token, string(kind), namespace)},
		})
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).
		WithIndex(&infrav1alpha1.Receiver{}, controller.ReceiverPathIndex, controller.IndexReceiverPath).Build()
	return &Server{Client: c, Now: func() time.Time { return now }}, c
}

func workspace(name, team string) *infrav1alpha1.Workspace {
	return &infrav1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{"team": team}}}
}

func refreshed(t *testing.T, c client.Client, name string) bool {
	t.Helper()
	ws := &infrav1alpha1.Workspace{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, ws); err != nil {
		t.Fatal(err)
	}
	return ws.Annotations[infrav1alpha1.RefreshRequestedAtAnnotation] == now.Format(time.RFC3339Nano)
}

func call(server *Server, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)
	return rec
}

func githubSignature(body string) string {
	mac := hmac.New(sha256.New, []byte(token))
	_, _ = mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhooks(t *testing.T) {
	path := func(kind string) string { return controller.ReceiverWebhookPath(token, kind, namespace) }
	const body = `{"ref":"refs/heads/main"}`

	for name, tc := range map[string]struct {
		receiver string
		headers  map[string]string
		status   int
		refresh  bool
	}{
		"github push": {
			receiver: "github", status: http.StatusOK, refresh: true,
			headers: map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSignature(body)},
		},
		"github ping": {
			receiver: "github", status: http.StatusOK,
			headers: map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": githubSignature(body)},
		},
		"github bad signature": {
			receiver: "github", status: http.StatusUnauthorized,
			headers: map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": githubSignature("another body")},
		},
		"gitlab push": {
			receiver: "gitlab", status: http.StatusOK, refresh: true,
			headers: map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": token},
		},
		"gitlab bad token": {
			receiver: "gitlab", status: http.StatusUnauthorized,
			headers: map[string]string{"X-Gitlab-Event": "Push Hook", "X-Gitlab-Token": "wrong"},
		},
		"generic": {
			receiver: "generic", status: http.StatusOK, refresh: true,
			headers: map[string]string{"Authorization": "Bearer " + token},
		},
		"generic without token": {receiver: "generic", status: http.StatusUnauthorized},
	} {
		t.Run(name, func(t *testing.T) {
			server, c := newServer(t)
			rec := call(server, http.MethodPost, path(tc.receiver), body, tc.headers)
			if rec.Code != tc.status {
				t.Fatalf("got %d (%s), want %d", rec.Code, rec.Body, tc.status)
			}
			if got := refreshed(t, c, "vpc") && refreshed(t, c, "dns"); got != tc.refresh {
				t.Fatalf("selected Workspaces refreshed: %v, want %v", got, tc.refresh)
			}
			if refreshed(t, c, "other") {
				t.Fatal("a Workspace outside of the selector must not be refreshed")
			}
		})
	}
}

func TestWebhookRouting(t *testing.T) {
	server, _ := newServer(t)
	if rec := call(server, http.MethodPost, "/hook/unknown", "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: got %d", rec.Code)
	}
	path := controller.ReceiverWebhookPath(token, "generic", namespace)
	if rec := call(server, http.MethodGet, path, "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d", rec.Code)
	}
}
