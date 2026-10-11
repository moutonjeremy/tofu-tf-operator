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

package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

const namespace = "infra"

// webhookServer records the requests it receives and answers with the given status codes in turn.
type webhookServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []*http.Request
	payloads []Payload
	statuses []int
	received chan struct{}
}

func newWebhookServer(t *testing.T, statuses ...int) *webhookServer {
	t.Helper()
	s := &webhookServer{statuses: statuses, received: make(chan struct{}, 10)}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p Payload
		_ = json.NewDecoder(r.Body).Decode(&p)
		s.mu.Lock()
		s.requests = append(s.requests, r)
		s.payloads = append(s.payloads, p)
		status := http.StatusOK
		if len(s.statuses) > 0 {
			status, s.statuses = s.statuses[0], s.statuses[1:]
		}
		s.mu.Unlock()
		w.WriteHeader(status)
		s.received <- struct{}{}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *webhookServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func newNotifier(t *testing.T, objects ...client.Object) *WebhookNotifier {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := infrav1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &WebhookNotifier{Client: c, HTTPClient: &http.Client{}, Backoff: time.Millisecond}
}

func provider(name, address string, mutate func(*infrav1alpha1.NotificationProvider)) *infrav1alpha1.NotificationProvider {
	p := &infrav1alpha1.NotificationProvider{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       infrav1alpha1.NotificationProviderSpec{Type: infrav1alpha1.ProviderWebhook, Address: address},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

func testObjects() (*infrav1alpha1.Workspace, *infrav1alpha1.Run) {
	ws := &infrav1alpha1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "vpc", Namespace: namespace}}
	run := &infrav1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "vpc-3", Namespace: namespace},
		Spec: infrav1alpha1.RunSpec{
			Operation: infrav1alpha1.OperationApply,
			Trigger:   infrav1alpha1.TriggerSourceChange,
			Revision:  "main@sha1:abc",
		},
		Status: infrav1alpha1.RunStatus{
			Phase: infrav1alpha1.PhaseAwaitingApproval,
			Plan:  &infrav1alpha1.PlanSummary{HasChanges: true, Add: 2, Change: 1},
		},
	}
	return ws, run
}

func TestSendPostsThePayload(t *testing.T) {
	server := newWebhookServer(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hook", Namespace: namespace},
		Data:       map[string][]byte{TokenKey: []byte("s3cr3t\n")},
	}
	n := newNotifier(t, secret, provider("ops", server.URL, func(p *infrav1alpha1.NotificationProvider) {
		p.Spec.SecretRef = &corev1.LocalObjectReference{Name: "hook"}
	}))
	ws, run := testObjects()

	payload := NewPayload(ws, run, infrav1alpha1.EventApprovalRequired, time.Now())
	if err := n.Send(context.Background(), namespace, "ops", payload); err != nil {
		t.Fatal(err)
	}
	req, got := server.requests[0], server.payloads[0]
	if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("unexpected request %s %v", req.Method, req.Header)
	}
	if auth := req.Header.Get("Authorization"); auth != "Bearer s3cr3t" {
		t.Fatalf("unexpected Authorization header %q", auth)
	}
	if got.Event != infrav1alpha1.EventApprovalRequired || got.Workspace.Name != "vpc" || got.Run.Name != "vpc-3" ||
		got.Run.Revision != "main@sha1:abc" || got.Plan.Add != 2 || !strings.Contains(got.ApproveCommand, "patch run vpc-3") {
		t.Fatalf("unexpected payload %+v", got)
	}
}

func TestSendUsesTheAddressOfTheSecret(t *testing.T) {
	server := newWebhookServer(t)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "hook", Namespace: namespace},
		Data:       map[string][]byte{AddressKey: []byte(server.URL)},
	}
	n := newNotifier(t, secret, provider("ops", "", func(p *infrav1alpha1.NotificationProvider) {
		p.Spec.SecretRef = &corev1.LocalObjectReference{Name: "hook"}
	}))
	ws, run := testObjects()
	if err := n.Send(context.Background(), namespace, "ops", NewPayload(ws, run, infrav1alpha1.EventPlanHasChanges, time.Now())); err != nil {
		t.Fatal(err)
	}
	if server.count() != 1 {
		t.Fatalf("expected one request, got %d", server.count())
	}
}

func TestSendRetries(t *testing.T) {
	ws, run := testObjects()
	payload := NewPayload(ws, run, infrav1alpha1.EventApplyFailed, time.Now())

	server := newWebhookServer(t, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusOK)
	n := newNotifier(t, provider("ops", server.URL, nil))
	if err := n.Send(context.Background(), namespace, "ops", payload); err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if server.count() != 3 {
		t.Fatalf("expected 3 attempts, got %d", server.count())
	}

	failing := newWebhookServer(t, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusInternalServerError, http.StatusOK)
	n = newNotifier(t, provider("ops", failing.URL, nil))
	if err := n.Send(context.Background(), namespace, "ops", payload); err == nil || failing.count() != attempts {
		t.Fatalf("expected a failure after %d attempts, got %v after %d", attempts, err, failing.count())
	}

	rejected := newWebhookServer(t, http.StatusUnauthorized)
	n = newNotifier(t, provider("ops", rejected.URL, nil))
	if err := n.Send(context.Background(), namespace, "ops", payload); err == nil || rejected.count() != 1 {
		t.Fatalf("a client error must not be retried, got %v after %d attempts", err, rejected.count())
	}
}

func TestSendSkipsSuspendedProviders(t *testing.T) {
	server := newWebhookServer(t)
	n := newNotifier(t, provider("ops", server.URL, func(p *infrav1alpha1.NotificationProvider) { p.Spec.Suspend = true }))
	ws, run := testObjects()
	if err := n.Send(context.Background(), namespace, "ops", NewPayload(ws, run, infrav1alpha1.EventApplyFailed, time.Now())); err != nil {
		t.Fatal(err)
	}
	if server.count() != 0 {
		t.Fatal("a suspended provider must not receive events")
	}
}

func TestNotifyFiltersEvents(t *testing.T) {
	all := newWebhookServer(t)
	failures := newWebhookServer(t)
	n := newNotifier(t, provider("all", all.URL, nil), provider("failures", failures.URL, nil))
	ws, run := testObjects()
	ws.Spec.Notifications = []infrav1alpha1.NotificationSpec{
		{ProviderRef: corev1.LocalObjectReference{Name: "all"}},
		{ProviderRef: corev1.LocalObjectReference{Name: "failures"}, Events: []infrav1alpha1.NotificationEvent{infrav1alpha1.EventApplyFailed}},
	}

	n.Notify(context.Background(), ws, run, infrav1alpha1.EventApplySucceeded)
	n.Notify(context.Background(), ws, run, infrav1alpha1.EventApplyFailed)
	for _, wait := range []chan struct{}{all.received, all.received, failures.received} {
		select {
		case <-wait:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for a notification")
		}
	}
	time.Sleep(50 * time.Millisecond)
	if all.count() != 2 || failures.count() != 1 || failures.payloads[0].Event != infrav1alpha1.EventApplyFailed {
		t.Fatalf("unexpected deliveries: all=%d failures=%d", all.count(), failures.count())
	}
}

func TestPayloadOfAFailure(t *testing.T) {
	ws, run := testObjects()
	run.Status.Phase = infrav1alpha1.PhaseFailed
	run.Status.Message = strings.Repeat("x", 3*maxMessageSize) + "the end"
	p := NewPayload(ws, run, infrav1alpha1.EventApplyFailed, time.Now())
	if len(p.Message) > maxMessageSize+3 || !strings.HasSuffix(p.Message, "the end") || p.ApproveCommand != "" {
		t.Fatalf("unexpected payload message (%d bytes) or approve command %q", len(p.Message), p.ApproveCommand)
	}
}
