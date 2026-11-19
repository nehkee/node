package test

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// PostgresEnv names a PostgreSQL URL whose role may CREATE DATABASE.
// PostgreSQL integration tests skip when it is unset.
const PostgresEnv = "LIVEPEER_TEST_POSTGRES_URL"

// PostgresURL returns the configured server URL or skips the test.
func PostgresURL(t testing.TB) *url.URL {
	t.Helper()
	raw := os.Getenv(PostgresEnv)
	if raw == "" {
		t.Skip(PostgresEnv + " is not set")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// PostgresDatabase provisions an empty database for t and drops it afterwards,
// returning a URL for it with the configured credentials.
func PostgresDatabase(t testing.TB) *url.URL {
	t.Helper()
	admin := PostgresURL(t)
	config, err := pgx.ParseConfig(admin.String())
	require.NoError(t, err)
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "lp_test_" + hex.EncodeToString(suffix)
	_, err = db.ExecContext(t.Context(), "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		require.NoError(t, err)
	})
	u := *admin
	u.Path = "/" + name
	return &u
}

// TerminatePostgresSessions ends every other session connected to the
// database named by u, modelling a server restart or network partition.
func TerminatePostgresSessions(t testing.TB, u *url.URL) {
	t.Helper()
	config, err := pgx.ParseConfig(PostgresURL(t).String())
	require.NoError(t, err)
	db := stdlib.OpenDB(*config)
	defer db.Close()
	_, err = db.ExecContext(t.Context(), "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()", u.Path[1:])
	require.NoError(t, err)
}
