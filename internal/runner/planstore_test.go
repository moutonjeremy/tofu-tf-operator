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
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
	"k8s.io/client-go/kubernetes/fake"

	infrav1alpha1 "github.com/labbs/tofu-tf-operator/api/v1alpha1"
)

func planStoreConfig(t *testing.T, storage PlanStorageConfig, credentials map[string]string) Config {
	t.Helper()
	dir := t.TempDir()
	for name, value := range credentials {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	storage.CredentialsDir = dir
	return Config{
		Namespace:     testNamespace,
		WorkspaceName: testWorkspace,
		RunName:       "vpc-3",
		RunUID:        "4b1c7c2e-run-uid",
		PlanStorage:   storage,
	}
}

// exercisePlanStore saves, reads and deletes a plan.
func exercisePlanStore(t *testing.T, store PlanStore) {
	t.Helper()
	ctx := context.Background()
	plan := bytes.Repeat([]byte("plan content "), 1000)
	if err := store.SavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	// Saving twice replaces the plan.
	if err := store.SavePlan(ctx, plan); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadPlan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, plan) {
		t.Fatalf("the loaded plan differs (%d bytes, want %d)", len(loaded), len(plan))
	}
	if err := store.DeletePlan(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.LoadPlan(ctx); err == nil {
		t.Fatal("the plan should be gone")
	}
}

func TestS3PlanStore(t *testing.T) {
	backend := s3mem.New()
	server := httptest.NewServer(gofakes3.New(backend).Server())
	defer server.Close()
	if err := backend.CreateBucket("plans"); err != nil {
		t.Fatal(err)
	}

	cfg := planStoreConfig(t, PlanStorageConfig{
		Type:        infrav1alpha1.PlanStorageS3,
		S3Bucket:    "plans",
		S3Prefix:    "tofu/",
		S3Region:    "us-east-1",
		S3Endpoint:  server.URL,
		S3PathStyle: true,
	}, map[string]string{S3AccessKeyIDKey: "access", S3SecretAccessKeyKey: "secret"})

	store, err := NewPlanStore(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s3, ok := store.(*S3PlanStore)
	if !ok {
		t.Fatalf("expected an S3 store, got %T", store)
	}
	if want := "tofu/infra/vpc/vpc-3-4b1c7c2e-run-uid.tfplan.gz"; s3.Key != want {
		t.Fatalf("unexpected key %q, want %q", s3.Key, want)
	}

	ctx := context.Background()
	if err := s3.SavePlan(ctx, []byte("plan")); err != nil {
		t.Fatal(err)
	}
	info, err := s3.Client.StatObject(ctx, "plans", s3.Key, minio.StatObjectOptions{})
	if err != nil || info.ContentType != "application/gzip" {
		t.Fatalf("unexpected object %+v, %v", info, err)
	}
	exercisePlanStore(t, s3)
}

func TestPostgresPlanStore(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set TEST_POSTGRES_URL to test the Postgres plan storage")
	}
	cfg := planStoreConfig(t, PlanStorageConfig{Type: infrav1alpha1.PlanStoragePostgres, PostgresTable: "test_plans"},
		map[string]string{PostgresURLKey: url})
	store, err := NewPlanStore(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	exercisePlanStore(t, store)
}

func TestPostgresPlanStoreRequiresURL(t *testing.T) {
	cfg := planStoreConfig(t, PlanStorageConfig{Type: infrav1alpha1.PlanStoragePostgres}, nil)
	if _, err := NewPlanStore(context.Background(), cfg, nil); err == nil || !strings.Contains(err.Error(), `"url"`) {
		t.Fatalf("expected a missing URL error, got %v", err)
	}
}

func TestPostgresErrorsHideTheURL(t *testing.T) {
	cfg := planStoreConfig(t, PlanStorageConfig{Type: infrav1alpha1.PlanStoragePostgres},
		map[string]string{PostgresURLKey: "postgres://user:s3cr3t@127.0.0.1:1/db?connect_timeout=1"})
	store, err := NewPlanStore(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = store.SavePlan(context.Background(), []byte("plan"))
	if err == nil || strings.Contains(err.Error(), "s3cr3t") || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("expected an error explaining the cause without the password, got %v", err)
	}
}

func TestSecretPlanStore(t *testing.T) {
	cfg := planStoreConfig(t, PlanStorageConfig{Type: infrav1alpha1.PlanStorageSecret}, nil)
	store, err := NewPlanStore(context.Background(), cfg, fake.NewClientset().CoreV1())
	if err != nil {
		t.Fatal(err)
	}
	exercisePlanStore(t, store)
}
