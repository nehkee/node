package redeemer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/livepeer/node/internal/test"
	"github.com/livepeer/node/nodeconfig/migrations"
	"github.com/livepeer/node/redemption"
	"github.com/stretchr/testify/require"
)

func postgresParams(t *testing.T, f *test.RedemptionRPC) Params {
	t.Helper()
	p := serviceParams(t, f)
	p.RedeemerDBBackend = redemption.BackendPostgres
	p.RedeemerPostgresURL = test.PostgresDatabase(t)
	p.RedeemerPostgresMaxConns = 4
	return p
}

func TestPostgresBackendValidation(t *testing.T) {
	p := serviceParams(t, test.NewRedemptionRPC(t))
	p.RedeemerDBBackend = redemption.BackendPostgres
	require.ErrorContains(t, p.Validate(), "requires a connection URL")
	p.RedeemerDBBackend = "mysql"
	require.ErrorContains(t, p.Validate(), "unsupported")
	p.RedeemerDBBackend = redemption.BackendSQLite
	require.NoError(t, p.Validate())
	p.RedeemerDBBackend = ""
	require.NoError(t, p.Validate(), "existing configurations without a backend stay on SQLite")
}

func TestPrintConfigRedactsPostgresURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LIVEPEER_REDEEMER_REDEEMER_DB_BACKEND", "postgres")
	t.Setenv("LIVEPEER_REDEEMER_REDEEMER_POSTGRES_URL", "postgres://redeemer:pg-secret@db.example:5432/redemption?sslmode=verify-full")
	var out, errOut bytes.Buffer
	cmd := Root(&out, &errOut)
	cmd.SetArgs([]string{"--config=", "--print-config"})
	require.NoError(t, cmd.Execute())
	require.Contains(t, out.String(), `RedeemerDBBackend = "postgres"`)
	require.Contains(t, out.String(), "postgres://redeemer@db.example:5432/redemption")
	require.NotContains(t, out.String(), "pg-secret")
}

// The standalone redeemer queues tickets durably in PostgreSQL, submits them,
// and resumes the same signed transaction after a restart.
func TestPostgresServeQueuesAndResumes(t *testing.T) {
	f := test.NewRedemptionRPC(t)
	p := postgresParams(t, f)
	seedBusyQueue(t, p)
	serve := func(until func() bool) {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- Serve(ctx, p, io.Discard) }()
		require.Eventually(t, until, 30*time.Second, 20*time.Millisecond)
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("redeemer did not stop")
		}
	}
	serve(func() bool { return len(f.Transactions()) > 0 })
	require.NoFileExists(t, p.RedeemerDB, "the postgres backend never creates the SQLite file")
	sent := len(f.Transactions())
	store, err := p.Database().OpenStore(t.Context())
	require.NoError(t, err)
	pending, err := store.SubmittedRedemptions()
	require.NoError(t, err)
	require.NoError(t, store.Close())
	require.NotEmpty(t, pending, "submitted redemptions are durable")

	// Restart: the redeemer reacquires ownership and resumes the pending work
	// instead of signing new transactions for the same tickets.
	for _, tx := range f.Transactions() {
		f.Mine(tx, true)
	}
	serve(func() bool {
		s, err := p.Database().OpenStore(t.Context())
		if err != nil {
			return false
		}
		defer s.Close()
		items, err := s.SubmittedRedemptions()
		return err == nil && len(items) < len(pending)
	})
	require.Equal(t, sent, len(f.Transactions()), "restart does not resend completed work")
}

func TestPostgresRedemptionsCommand(t *testing.T) {
	u := test.PostgresDatabase(t)
	file := filepath.Join(t.TempDir(), "postgres-url")
	require.NoError(t, os.WriteFile(file, []byte(u.String()), 0600))
	var out, errOut bytes.Buffer
	cmd := Root(&out, &errOut)
	cmd.SetArgs([]string{"redemptions", "--config=", "--data-dir", t.TempDir(), "--network", "test", "--redeemer-db-backend", "postgres", "--redeemer-postgres-url-file", file})
	require.NoError(t, cmd.Execute(), errOut.String())
	require.JSONEq(t, "[]", out.String())
}

func TestPostgresMigrateCommand(t *testing.T) {
	u := test.PostgresDatabase(t)
	file := filepath.Join(t.TempDir(), "postgres-url")
	require.NoError(t, os.WriteFile(file, []byte(u.String()), 0600))
	run := func(action string) []migrations.Status {
		var out, errOut bytes.Buffer
		cmd := Root(&out, &errOut)
		cmd.SetArgs([]string{"migrate", action, "--config=", "--data-dir", t.TempDir(), "--network", "test", "--redeemer-db-backend", "postgres", "--redeemer-postgres-url-file", file})
		require.NoError(t, cmd.Execute(), errOut.String())
		var status []migrations.Status
		require.NoError(t, json.Unmarshal(out.Bytes(), &status))
		require.NotEmpty(t, status)
		return status
	}
	require.False(t, run("status")[0].Applied)
	require.True(t, run("up")[0].Applied)
	require.True(t, run("status")[0].Applied)
	require.False(t, run("down")[0].Applied)
}
