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

// Package receiver serves the webhooks of the Receivers: a call makes the selected
// Workspaces check their source right away, instead of waiting for the next poll.
package receiver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
	"github.com/labbs/tofu-tf-operator/internal/controller"
)

const maxBodySize = 1 << 20

// internalError hides the details of internal failures from the callers; they are logged.
var internalError = response{Message: "internal error"}

// Server serves the webhook calls of the Receivers.
type Server struct {
	Client client.Client
	// Addr is the address to listen on, e.g. ":9292".
	Addr string
	// Now defaults to time.Now.
	Now func() time.Time
}

// NeedLeaderElection lets every replica serve the webhooks: a Service may route
// a call to any of them.
func (s *Server) NeedLeaderElection() bool {
	return false
}

// Start serves until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.Addr,
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
	}
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	logf.FromContext(ctx).Info("serving the Receiver webhooks", "address", s.Addr)
	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

// response is the body of the answers to webhook calls.
type response struct {
	Message    string   `json:"message"`
	Workspaces []string `json:"workspaces,omitempty"`
}

func reply(w http.ResponseWriter, status int, r response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(r)
}

// ServeHTTP authenticates a call and refreshes the Workspaces of its Receiver.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	log := logf.FromContext(ctx).WithValues("path", req.URL.Path)
	if req.Method != http.MethodPost {
		reply(w, http.StatusMethodNotAllowed, response{Message: "use POST"})
		return
	}

	// The path, derived from the token, identifies the Receiver.
	receivers := &infrav1alpha1.ReceiverList{}
	if err := s.Client.List(ctx, receivers, client.MatchingFields{controller.ReceiverPathIndex: req.URL.Path}); err != nil {
		log.Error(err, "listing the Receivers")
		reply(w, http.StatusInternalServerError, internalError)
		return
	}
	if len(receivers.Items) != 1 {
		reply(w, http.StatusNotFound, response{Message: "not found"})
		return
	}
	receiver := &receivers.Items[0]

	body, err := io.ReadAll(io.LimitReader(req.Body, maxBodySize))
	if err != nil {
		reply(w, http.StatusBadRequest, response{Message: "cannot read the body"})
		return
	}
	token, err := s.token(ctx, receiver)
	if err != nil {
		log.Error(err, "reading the token of the Receiver", "receiver", receiver.Name)
		reply(w, http.StatusInternalServerError, internalError)
		return
	}
	trigger, err := authenticate(receiver.Spec.Type, token, req.Header, body)
	if err != nil {
		reply(w, http.StatusUnauthorized, response{Message: err.Error()})
		return
	}
	if !trigger {
		reply(w, http.StatusOK, response{Message: "event ignored"})
		return
	}

	refreshed, err := s.refresh(ctx, receiver)
	if err != nil {
		log.Error(err, "refreshing the Workspaces", "receiver", receiver.Name)
		reply(w, http.StatusInternalServerError, internalError)
		return
	}
	log.Info("webhook received", "receiver", receiver.Name, "workspaces", refreshed)
	reply(w, http.StatusOK, response{Message: "source check requested", Workspaces: refreshed})
}

func (s *Server) token(ctx context.Context, receiver *infrav1alpha1.Receiver) (string, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: receiver.Namespace, Name: receiver.Spec.SecretRef.Name}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(secret.Data[infrav1alpha1.ReceiverTokenKey]))
	if token == "" {
		return "", fmt.Errorf("the Secret %s has no token", key)
	}
	return token, nil
}

// authenticate checks a call and tells whether its event should refresh the Workspaces.
func authenticate(kind infrav1alpha1.ReceiverType, token string, header http.Header, body []byte) (bool, error) {
	switch kind {
	case infrav1alpha1.ReceiverGitHub:
		signature, ok := strings.CutPrefix(header.Get("X-Hub-Signature-256"), "sha256=")
		mac := hmac.New(sha256.New, []byte(token))
		_, _ = mac.Write(body)
		expected := hex.EncodeToString(mac.Sum(nil))
		if !ok || !hmac.Equal([]byte(signature), []byte(expected)) {
			return false, errors.New("invalid signature")
		}
		// ping is sent when the webhook is created.
		return header.Get("X-GitHub-Event") == "push", nil

	case infrav1alpha1.ReceiverGitLab:
		if subtle.ConstantTimeCompare([]byte(header.Get("X-Gitlab-Token")), []byte(token)) != 1 {
			return false, errors.New("invalid token")
		}
		event := header.Get("X-Gitlab-Event")
		return event == "Push Hook" || event == "Tag Push Hook", nil

	case infrav1alpha1.ReceiverGeneric:
		bearer, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(bearer), []byte(token)) != 1 {
			return false, errors.New("invalid token")
		}
		return true, nil
	}
	return false, fmt.Errorf("unknown receiver type %q", kind)
}

// refresh asks the selected Workspaces to check their source.
func (s *Server) refresh(ctx context.Context, receiver *infrav1alpha1.Receiver) ([]string, error) {
	selector := labels.Everything()
	if receiver.Spec.WorkspaceSelector != nil {
		var err error
		if selector, err = metav1.LabelSelectorAsSelector(receiver.Spec.WorkspaceSelector); err != nil {
			return nil, err
		}
	}
	workspaces := &infrav1alpha1.WorkspaceList{}
	if err := s.Client.List(ctx, workspaces, client.InNamespace(receiver.Namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, err
	}

	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	value := now().UTC().Format(time.RFC3339Nano)
	refreshed := make([]string, 0, len(workspaces.Items))
	for i := range workspaces.Items {
		ws := &workspaces.Items[i]
		patch := client.MergeFrom(ws.DeepCopy())
		if ws.Annotations == nil {
			ws.Annotations = map[string]string{}
		}
		ws.Annotations[infrav1alpha1.RefreshRequestedAtAnnotation] = value
		if err := s.Client.Patch(ctx, ws, patch); client.IgnoreNotFound(err) != nil {
			return refreshed, err
		}
		refreshed = append(refreshed, ws.Name)
	}
	return refreshed, nil
}
