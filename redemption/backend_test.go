package redemption

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"
)

// postgresTestEnv names a PostgreSQL URL whose role may CREATE DATABASE.
// Each test gets a fresh database; without the variable PostgreSQL tests skip.
const postgresTestEnv = "LIVEPEER_TEST_POSTGRES_URL"

// testBackend selects the backend for shared store helpers. Tests in this
// package are sequential, so runBackends may switch it per subtest.
var testBackend = BackendSQLite

var testDatabases = map[*RedeemerDB]Database{}

func postgresTestURL(t testing.TB) *url.URL {
	t.Helper()
	raw := os.Getenv(postgresTestEnv)
	if raw == "" {
		t.Skip(postgresTestEnv + " is not set")
	}
	u, err := url.Parse(raw)
	require.NoError(t, err)
	return u
}

// newPostgresTestDatabase provisions an empty database and drops it afterwards.
func newPostgresTestDatabase(t testing.TB) Database {
	t.Helper()
	admin := postgresTestURL(t)
	config, err := pgx.ParseConfig(admin.String())
	require.NoError(t, err)
	db := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = db.Close() })
	suffix := make([]byte, 6)
	_, _ = rand.Read(suffix)
	name := "lp_redemption_" + hex.EncodeToString(suffix)
	_, err = db.ExecContext(t.Context(), "CREATE DATABASE "+name)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, err := db.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)")
		require.NoError(t, err)
	})
	u := *admin
	u.Path = "/" + name
	return Database{Backend: BackendPostgres, PostgresURL: &u, PostgresMaxConns: 4}
}

func testDatabase(t testing.TB) Database {
	t.Helper()
	if testBackend == BackendPostgres {
		return newPostgresTestDatabase(t)
	}
	return Database{Backend: BackendSQLite, Path: filepath.Join(t.TempDir(), "tickets.sqlite")}
}

func openTestDatabase(t testing.TB, d Database) (*RedeemerDB, error) {
	t.Helper()
	s, err := d.OpenStore(t.Context())
	if err == nil {
		testDatabases[s] = d
	}
	return s, err
}

// openTestRedeemerDB opens an empty store on the current test backend.
func openTestRedeemerDB(t testing.TB) (*RedeemerDB, error) {
	t.Helper()
	return openTestDatabase(t, testDatabase(t))
}

// testDatabaseOf returns the configuration that opened s, for restarts.
func testDatabaseOf(t testing.TB, s *RedeemerDB) Database {
	t.Helper()
	d, ok := testDatabases[s]
	require.True(t, ok, "store was not opened by a backend test helper")
	return d
}

// failWrites injects a storage failure: a trigger aborting matching writes on
// table with message. event is "INSERT" or "UPDATE OF column"; when is an
// optional condition over NEW. Each backend receives its own trigger syntax.
func failWrites(t testing.TB, s *RedeemerDB, name, event, table, when, message string) error {
	t.Helper()
	if !s.db.postgres() {
		condition := ""
		if when != "" {
			condition = "WHEN " + when + " "
		}
		_, err := s.db.Exec("CREATE TRIGGER " + name + " BEFORE " + event + " ON " + table + " " + condition + "BEGIN SELECT RAISE(ABORT,'" + message + "'); END")
		return err
	}
	if _, err := s.db.Exec("CREATE FUNCTION " + name + "() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION '" + message + "'; END $$"); err != nil {
		return err
	}
	condition := ""
	if when != "" {
		condition = "WHEN (" + when + ") "
	}
	_, err := s.db.Exec("CREATE TRIGGER " + name + " BEFORE " + event + " ON " + table + " FOR EACH ROW " + condition + "EXECUTE FUNCTION " + name + "()")
	return err
}

func allowWrites(t testing.TB, s *RedeemerDB, name, table string) error {
	t.Helper()
	if !s.db.postgres() {
		_, err := s.db.Exec("DROP TRIGGER " + name)
		return err
	}
	if _, err := s.db.Exec("DROP TRIGGER " + name + " ON " + table); err != nil {
		return err
	}
	_, err := s.db.Exec("DROP FUNCTION " + name + "()")
	return err
}

// runBackends runs fn against SQLite and, when configured, PostgreSQL.
func runBackends(t *testing.T, fn func(t *testing.T)) {
	for _, backend := range []string{BackendSQLite, BackendPostgres} {
		t.Run(backend, func(t *testing.T) {
			if backend == BackendPostgres {
				postgresTestURL(t)
			}
			previous := testBackend
			testBackend = backend
			t.Cleanup(func() { testBackend = previous })
			fn(t)
		})
	}
}
