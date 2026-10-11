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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

const (
	// PlanSecretKey is the key of the gzipped plan file in the plan Secret.
	PlanSecretKey = "tfplan.gz"
	// maxPlanSize leaves room for the metadata of the Secret under the 1 MiB limit.
	maxPlanSize = 1000 * 1024
)

// PlanSecretName is the name of the Secret holding the plan of a Run.
func PlanSecretName(runName string) string {
	return runName + "-plan"
}

// Store persists the artifacts of a Run between the plan and apply steps.
type Store interface {
	SavePlan(ctx context.Context, plan []byte) error
	LoadPlan(ctx context.Context) ([]byte, error)
	SaveOutputs(ctx context.Context, outputs map[string][]byte) error
	DeleteOutputs(ctx context.Context) error
}

// SecretStore stores the artifacts of a Run in Secrets of its namespace.
type SecretStore struct {
	Secrets corev1client.SecretsGetter
	Config  Config
}

var _ Store = &SecretStore{}

// SavePlan stores the gzipped plan in a Secret owned by the Run.
func (s *SecretStore) SavePlan(ctx context.Context, plan []byte) error {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plan); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if buf.Len() > maxPlanSize {
		return fmt.Errorf("the plan is too large to be stored in a Secret (%d bytes compressed, limit %d)", buf.Len(), maxPlanSize)
	}

	secret := &corev1.Secret{
		ObjectMeta: s.meta(PlanSecretName(s.Config.RunName), "Run", s.Config.RunName, s.Config.RunUID),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{PlanSecretKey: buf.Bytes()},
	}
	return s.upsert(ctx, secret)
}

// LoadPlan reads the plan stored by SavePlan.
func (s *SecretStore) LoadPlan(ctx context.Context) ([]byte, error) {
	secret, err := s.Secrets.Secrets(s.Config.Namespace).Get(ctx, PlanSecretName(s.Config.RunName), metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(secret.Data[PlanSecretKey]))
	if err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// SaveOutputs replaces the content of the outputs Secret, owned by the Workspace.
func (s *SecretStore) SaveOutputs(ctx context.Context, outputs map[string][]byte) error {
	secret := &corev1.Secret{
		ObjectMeta: s.meta(s.Config.OutputsSecret, "Workspace", s.Config.WorkspaceName, s.Config.WorkspaceUID),
		Type:       corev1.SecretTypeOpaque,
		Data:       outputs,
	}
	return s.upsert(ctx, secret)
}

// DeleteOutputs removes the outputs Secret.
func (s *SecretStore) DeleteOutputs(ctx context.Context) error {
	err := s.Secrets.Secrets(s.Config.Namespace).Delete(ctx, s.Config.OutputsSecret, metav1.DeleteOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *SecretStore) meta(name, ownerKind, ownerName, ownerUID string) metav1.ObjectMeta {
	meta := metav1.ObjectMeta{
		Name:      name,
		Namespace: s.Config.Namespace,
		Labels: map[string]string{
			infrav1alpha1.WorkspaceLabel: s.Config.WorkspaceName,
			infrav1alpha1.RunLabel:       s.Config.RunName,
		},
	}
	if ownerUID != "" {
		meta.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: infrav1alpha1.GroupVersion.String(),
			Kind:       ownerKind,
			Name:       ownerName,
			UID:        types.UID(ownerUID),
		}}
	}
	return meta
}

func (s *SecretStore) upsert(ctx context.Context, secret *corev1.Secret) error {
	secrets := s.Secrets.Secrets(secret.Namespace)
	_, err := secrets.Create(ctx, secret, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	existing, err := secrets.Get(ctx, secret.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	existing.Labels = secret.Labels
	existing.OwnerReferences = secret.OwnerReferences
	existing.Data = secret.Data
	_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// outputsToData converts the result of `output -json` to Secret data:
// strings are written as is, other values as JSON.
func outputsToData(outputJSON []byte) (map[string][]byte, error) {
	var outputs map[string]struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(outputJSON, &outputs); err != nil {
		return nil, fmt.Errorf("decoding outputs: %w", err)
	}
	data := make(map[string][]byte, len(outputs))
	for name, output := range outputs {
		var str string
		if err := json.Unmarshal(output.Value, &str); err == nil {
			data[name] = []byte(str)
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, output.Value); err != nil {
			return nil, fmt.Errorf("encoding output %s: %w", name, err)
		}
		data[name] = compact.Bytes()
	}
	return data, nil
}
