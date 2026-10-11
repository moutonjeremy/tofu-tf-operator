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
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Keys of the Secret holding the S3 credentials.
const (
	S3AccessKeyIDKey     = "AWS_ACCESS_KEY_ID"
	S3SecretAccessKeyKey = "AWS_SECRET_ACCESS_KEY"
	S3SessionTokenKey    = "AWS_SESSION_TOKEN"

	defaultS3Endpoint = "https://s3.amazonaws.com"
	// maxS3PlanSize bounds the memory used to read a plan back.
	maxS3PlanSize = 1 << 30
)

// S3PlanStore keeps plans in a bucket of S3 or of a compatible service.
type S3PlanStore struct {
	Client *minio.Client
	Bucket string
	Key    string
}

var _ PlanStore = &S3PlanStore{}

// NewS3PlanStore connects to the bucket configured for the Run. The credentials come
// from the mounted Secret if any, otherwise from the environment or the ServiceAccount
// (IRSA, EKS pod identity).
func NewS3PlanStore(cfg Config) (*S3PlanStore, error) {
	storage := cfg.PlanStorage
	endpoint, err := url.Parse(cmpOr(storage.S3Endpoint, defaultS3Endpoint))
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", storage.S3Endpoint)
	}

	files, err := readOptionalFiles(storage.CredentialsDir, S3AccessKeyIDKey, S3SecretAccessKeyKey, S3SessionTokenKey)
	if err != nil {
		return nil, err
	}
	var creds *credentials.Credentials
	if accessKey := strings.TrimSpace(string(files[S3AccessKeyIDKey])); accessKey != "" {
		creds = credentials.NewStaticV4(accessKey,
			strings.TrimSpace(string(files[S3SecretAccessKeyKey])), strings.TrimSpace(string(files[S3SessionTokenKey])))
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{
			&credentials.EnvAWS{},
			&credentials.FileAWSCredentials{},
			&credentials.IAM{},
		})
	}

	lookup := minio.BucketLookupAuto
	if storage.S3PathStyle {
		lookup = minio.BucketLookupPath
	}
	client, err := minio.New(endpoint.Host, &minio.Options{
		Creds:        creds,
		Secure:       endpoint.Scheme == "https",
		Region:       storage.S3Region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, err
	}
	return &S3PlanStore{Client: client, Bucket: storage.S3Bucket, Key: storage.S3Prefix + planObjectName(cfg)}, nil
}

// SavePlan uploads the gzipped plan.
func (s *S3PlanStore) SavePlan(ctx context.Context, plan []byte) error {
	data, err := compress(plan)
	if err != nil {
		return err
	}
	_, err = s.Client.PutObject(ctx, s.Bucket, s.Key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: "application/gzip"})
	if err != nil {
		return fmt.Errorf("uploading the plan to s3://%s/%s: %w", s.Bucket, s.Key, err)
	}
	return nil
}

// LoadPlan downloads the plan stored by SavePlan.
func (s *S3PlanStore) LoadPlan(ctx context.Context) ([]byte, error) {
	object, err := s.Client.GetObject(ctx, s.Bucket, s.Key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("downloading the plan from s3://%s/%s: %w", s.Bucket, s.Key, err)
	}
	defer func() { _ = object.Close() }()
	data, err := io.ReadAll(io.LimitReader(object, maxS3PlanSize))
	if err != nil {
		return nil, fmt.Errorf("downloading the plan from s3://%s/%s: %w", s.Bucket, s.Key, err)
	}
	return decompress(data)
}

// DeletePlan removes the plan object.
func (s *S3PlanStore) DeletePlan(ctx context.Context) error {
	return s.Client.RemoveObject(ctx, s.Bucket, s.Key, minio.RemoveObjectOptions{})
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
