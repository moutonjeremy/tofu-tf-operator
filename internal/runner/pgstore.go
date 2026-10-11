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
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	// PostgresURLKey is the key of the connection URL in the Secret of the Postgres storage.
	PostgresURLKey = "url"
	// DefaultPostgresTable is the table of the plans when none is configured.
	DefaultPostgresTable = "tofu_plans"
	// postgresRetention removes the plans of the Runs that were never applied
	// (superseded or failed), which would otherwise stay forever.
	postgresRetention = "30 days"
)

// PostgresPlanStore keeps plans in a PostgreSQL table, created if needed.
type PostgresPlanStore struct {
	URL   string
	Table string
	cfg   Config
}

var _ PlanStore = &PostgresPlanStore{}

// NewPostgresPlanStore reads the connection URL from the mounted Secret.
func NewPostgresPlanStore(_ context.Context, cfg Config) (*PostgresPlanStore, error) {
	files, err := readOptionalFiles(cfg.PlanStorage.CredentialsDir, PostgresURLKey)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSpace(string(files[PostgresURLKey]))
	if url == "" {
		return nil, fmt.Errorf("the Secret of the Postgres plan storage has no %q key", PostgresURLKey)
	}
	return &PostgresPlanStore{URL: url, Table: cmpOr(cfg.PlanStorage.PostgresTable, DefaultPostgresTable), cfg: cfg}, nil
}

// connect opens a connection and makes sure the table exists.
func (s *PostgresPlanStore) connect(ctx context.Context) (*pgx.Conn, string, error) {
	conn, err := pgx.Connect(ctx, s.URL)
	if err != nil {
		return nil, "", fmt.Errorf("connecting to the Postgres plan storage: %s", s.redact(err))
	}
	table := pgx.Identifier{s.Table}.Sanitize()
	_, err = conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		run_uid    text PRIMARY KEY,
		namespace  text NOT NULL,
		workspace  text NOT NULL,
		run        text NOT NULL,
		plan       bytea NOT NULL,
		created_at timestamptz NOT NULL DEFAULT now()
	)`)
	if err == nil {
		index := pgx.Identifier{s.Table + "_created_at"}.Sanitize()
		_, err = conn.Exec(ctx, `CREATE INDEX IF NOT EXISTS `+index+` ON `+table+` (created_at)`)
	}
	if err != nil {
		_ = conn.Close(ctx)
		return nil, "", fmt.Errorf("creating the table %s: %w", s.Table, err)
	}
	return conn, table, nil
}

// redact removes the password of the connection URL from an error message.
func (s *PostgresPlanStore) redact(err error) string {
	message := err.Error()
	if config, parseErr := pgx.ParseConfig(s.URL); parseErr == nil && config.Password != "" {
		message = strings.ReplaceAll(message, config.Password, "xxxxx")
	} else if parseErr != nil {
		// An unparsable URL may be echoed in the error: say nothing more.
		return "invalid connection URL"
	}
	return message
}

// SavePlan inserts the gzipped plan and removes the old plans that were never applied.
func (s *PostgresPlanStore) SavePlan(ctx context.Context, plan []byte) error {
	data, err := compress(plan)
	if err != nil {
		return err
	}
	conn, table, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()

	_, err = conn.Exec(ctx, `INSERT INTO `+table+` (run_uid, namespace, workspace, run, plan) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (run_uid) DO UPDATE SET plan = EXCLUDED.plan, created_at = now()`,
		s.cfg.RunUID, s.cfg.Namespace, s.cfg.WorkspaceName, s.cfg.RunName, data)
	if err != nil {
		return fmt.Errorf("saving the plan: %w", err)
	}
	_, err = conn.Exec(ctx, `DELETE FROM `+table+` WHERE created_at < now() - $1::interval`, postgresRetention)
	return err
}

// LoadPlan reads the plan stored by SavePlan.
func (s *PostgresPlanStore) LoadPlan(ctx context.Context) ([]byte, error) {
	conn, table, err := s.connect(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var data []byte
	err = conn.QueryRow(ctx, `SELECT plan FROM `+table+` WHERE run_uid = $1`, s.cfg.RunUID).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("no plan stored for the run %s", s.cfg.RunName)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the plan: %w", err)
	}
	return decompress(data)
}

// DeletePlan removes the plan of the Run.
func (s *PostgresPlanStore) DeletePlan(ctx context.Context) error {
	conn, table, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, `DELETE FROM `+table+` WHERE run_uid = $1`, s.cfg.RunUID)
	return err
}
