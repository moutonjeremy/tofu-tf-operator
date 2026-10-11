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

// Package notify sends the events of Runs to the NotificationProviders of their Workspace.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/metrics"
)

// Keys of the Secret referenced by a NotificationProvider.
const (
	AddressKey = "address"
	TokenKey   = "token"
)

const (
	defaultTimeout = 15 * time.Second
	attempts       = 3
	userAgent      = "tofu-tf-operator"
	// maxMessageSize keeps the payload small; the full logs are in the Job.
	maxMessageSize = 2048
)

// Payload is the JSON body posted to webhooks.
type Payload struct {
	Event     infrav1alpha1.NotificationEvent `json:"event"`
	Timestamp time.Time                       `json:"timestamp"`
	Workspace ObjectReference                 `json:"workspace"`
	Run       RunInfo                         `json:"run"`
	Plan      *infrav1alpha1.PlanSummary      `json:"plan,omitempty"`
	Message   string                          `json:"message,omitempty"`
	// ApproveCommand is set when the Run awaits approval.
	ApproveCommand string `json:"approveCommand,omitempty"`
}

// ObjectReference identifies a namespaced object.
type ObjectReference struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// RunInfo describes the Run of an event.
type RunInfo struct {
	Name      string                     `json:"name"`
	Operation infrav1alpha1.RunOperation `json:"operation"`
	Trigger   infrav1alpha1.RunTrigger   `json:"trigger"`
	Phase     infrav1alpha1.RunPhase     `json:"phase"`
	Revision  string                     `json:"revision"`
}

// NewPayload builds the payload of an event.
func NewPayload(ws *infrav1alpha1.Workspace, run *infrav1alpha1.Run, event infrav1alpha1.NotificationEvent, now time.Time) Payload {
	p := Payload{
		Event:     event,
		Timestamp: now.UTC(),
		Workspace: ObjectReference{Namespace: ws.Namespace, Name: ws.Name},
		Run: RunInfo{
			Name:      run.Name,
			Operation: run.Spec.Operation,
			Trigger:   run.Spec.Trigger,
			Phase:     run.Status.Phase,
			Revision:  run.Spec.Revision,
		},
		Plan: run.Status.Plan,
	}
	if run.Status.Phase == infrav1alpha1.PhaseFailed {
		p.Message = run.Status.Message
		if len(p.Message) > maxMessageSize {
			p.Message = "..." + p.Message[len(p.Message)-maxMessageSize:]
		}
	}
	if event == infrav1alpha1.EventApprovalRequired {
		p.ApproveCommand = run.ApproveCommand()
	}
	return p
}

// WebhookNotifier posts events to the webhook NotificationProviders of a Workspace.
type WebhookNotifier struct {
	Client     client.Client
	HTTPClient *http.Client
	Recorder   events.EventRecorder
	// Backoff is the delay before the first retry; it doubles for the next one.
	Backoff time.Duration
}

// NewWebhookNotifier returns a notifier retrying after 1s then 2s.
func NewWebhookNotifier(c client.Client, recorder events.EventRecorder) *WebhookNotifier {
	return &WebhookNotifier{Client: c, HTTPClient: &http.Client{}, Recorder: recorder, Backoff: time.Second}
}

// Notify sends an event in the background, so that a slow endpoint never delays
// a reconciliation. Failures are reported as Warning events on the Workspace.
func (n *WebhookNotifier) Notify(ctx context.Context, ws *infrav1alpha1.Workspace, run *infrav1alpha1.Run, event infrav1alpha1.NotificationEvent) {
	payload := NewPayload(ws, run, event, time.Now())
	ws = ws.DeepCopy()
	for _, spec := range ws.Spec.Notifications {
		if len(spec.Events) > 0 && !slices.Contains(spec.Events, event) {
			continue
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer cancel()
			err := n.Send(ctx, ws.Namespace, spec.ProviderRef.Name, payload)
			result := "success"
			if err != nil {
				result = "failure"
			}
			metrics.NotificationsTotal.WithLabelValues(ws.Namespace, spec.ProviderRef.Name, result).Inc()
			if err != nil {
				logf.FromContext(ctx).Error(err, "sending a notification", "provider", spec.ProviderRef.Name, "event", event)
				if n.Recorder != nil {
					n.Recorder.Eventf(ws, nil, corev1.EventTypeWarning, "NotificationFailed", "Notify",
						"Sending %s to %s failed: %v", event, spec.ProviderRef.Name, err)
				}
			}
		}()
	}
}

// Send posts a payload to a provider, retrying on network errors and server errors.
func (n *WebhookNotifier) Send(ctx context.Context, namespace, providerName string, payload Payload) error {
	provider := &infrav1alpha1.NotificationProvider{}
	if err := n.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: providerName}, provider); err != nil {
		return fmt.Errorf("reading the NotificationProvider: %w", err)
	}
	if provider.Spec.Suspend {
		return nil
	}
	address, token, err := n.endpoint(ctx, provider)
	if err != nil {
		return err
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	timeout := defaultTimeout
	if provider.Spec.Timeout != nil && provider.Spec.Timeout.Duration > 0 {
		timeout = provider.Spec.Timeout.Duration
	}

	backoff := n.Backoff
	for attempt := 1; ; attempt++ {
		retry, err := n.post(ctx, address, token, body, timeout)
		if err == nil || !retry || attempt == attempts {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// endpoint returns the address and the token of a provider; the Secret takes precedence.
func (n *WebhookNotifier) endpoint(ctx context.Context, provider *infrav1alpha1.NotificationProvider) (string, string, error) {
	address, token := provider.Spec.Address, ""
	if ref := provider.Spec.SecretRef; ref != nil {
		secret := &corev1.Secret{}
		if err := n.Client.Get(ctx, types.NamespacedName{Namespace: provider.Namespace, Name: ref.Name}, secret); err != nil {
			return "", "", fmt.Errorf("reading the Secret of the NotificationProvider: %w", err)
		}
		if a := strings.TrimSpace(string(secret.Data[AddressKey])); a != "" {
			address = a
		}
		token = strings.TrimSpace(string(secret.Data[TokenKey]))
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("invalid webhook address")
	}
	return address, token, nil
}

// post sends one request and tells whether a failure is worth retrying.
func (n *WebhookNotifier) post(ctx context.Context, address, token string, body []byte, timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", userAgent)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	httpClient := n.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		// The address may hold a token: only keep the cause of the error.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return true, fmt.Errorf("posting the event: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return false, nil
	}
	retry := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
	return retry, fmt.Errorf("the webhook answered %s", resp.Status)
}
