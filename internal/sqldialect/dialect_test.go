package sqldialect

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite"
)

func TestRebind(t *testing.T) {
	for in, want := range map[string]string{
		"SELECT 1":                         "SELECT 1",
		"SELECT ? , ?":                     "SELECT $1 , $2",
		"WHERE a=?1 OR b=?1 AND c=?2":      "WHERE a=$1 OR b=$1 AND c=$2",
		"SELECT '?',\"a?\",? -- why?\n,?":  "SELECT '?',\"a?\",$1 -- why?\n,$2",
		"SELECT 'it''s ?', ?":              "SELECT 'it''s ?', $1",
		"WHERE (a,b)=(SELECT ?,?) AND c=?": "WHERE (a,b)=(SELECT $1,$2) AND c=$3",
		"SELECT 'unterminated ?":           "SELECT 'unterminated ?",
	} {
		require.Equal(t, want, PostgreSQL.Rebind(in), in)
		require.Equal(t, in, SQLite.Rebind(in), in)
	}
}

func TestOf(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	defer db.Close()
	require.Equal(t, SQLite, Of(db))
	pg, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
	require.NoError(t, err)
	defer pg.Close()
	require.Equal(t, PostgreSQL, Of(pg))
}
