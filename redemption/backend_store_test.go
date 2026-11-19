package redemption

import (
	"errors"
	"io/fs"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/node/internal/test"
	"github.com/livepeer/node/nodeconfig/migrations"
	"github.com/livepeer/node/pm"
	"github.com/stretchr/testify/require"
)

func TestBackendFreshInitAndMigrations(t *testing.T) {
	runBackends(t, func(t *testing.T) {
		d := testDatabase(t)
		s, err := openTestDatabase(t, d)
		require.NoError(t, err)
		files, err := fs.Sub(MigrationFiles, migrations.Dir(s.db.DB))
		require.NoError(t, err)
		status, err := migrations.List(t.Context(), s.db.DB, files)
		require.NoError(t, err)
		require.NotEmpty(t, status)
		for _, item := range status {
			require.True(t, item.Applied, item.Filename)
		}
		require.NoError(t, s.Close())
		// Reopening an initialized database applies nothing and keeps checksums.
		s, err = openTestDatabase(t, d)
		require.NoError(t, err)
		again, err := migrations.List(t.Context(), s.db.DB, files)
		require.NoError(t, err)
		require.Equal(t, status, again)
		if testBackend == BackendPostgres {
			// The PostgreSQL catalog rolls back and reapplies cleanly while empty.
			require.NoError(t, migrations.Down(t.Context(), s.db.DB, files))
			require.NoError(t, migrations.Up(t.Context(), s.db.DB, files))
		}
		require.NoError(t, s.Close())
	})
}

func TestBackendConcurrentAdmissionIsAtomic(t *testing.T) {
	runBackends(t, func(t *testing.T) {
		s, _ := boundStore(t, "redeemer", "chain", Limits{Pending: 100, PerRecipient: 5})
		duplicate, _ := winningTicket(t, 1)
		distinct := make([]*pm.SignedTicket, 20)
		for i := range distinct {
			distinct[i], _ = winningTicket(t, int64(i+2))
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		var admitted, refused int
		for range 8 {
			wg.Go(func() { require.NoError(t, s.Admit(t.Context(), duplicate)) })
		}
		wg.Wait()
		for _, ticket := range distinct {
			wg.Go(func() {
				err := s.Admit(t.Context(), ticket)
				mu.Lock()
				defer mu.Unlock()
				if errors.Is(err, ErrCapacity) {
					refused++
					return
				}
				require.NoError(t, err)
				admitted++
			})
		}
		wg.Wait()
		var rows, identities int
		require.NoError(t, s.db.QueryRow("SELECT count(*) FROM winning_tickets").Scan(&rows))
		require.NoError(t, s.db.QueryRow("SELECT count(*) FROM ticket_state").Scan(&identities))
		require.Equal(t, 5, rows, "duplicates collapse and the per-recipient limit is exact")
		require.Equal(t, 5, identities)
		require.Equal(t, 4, admitted)
		require.Equal(t, 16, refused)
	})
}

func TestBackendRejectedReceiptRollsBack(t *testing.T) {
	runBackends(t, func(t *testing.T) {
		s, _ := boundStore(t, "orchestrator", "chain", DefaultLimits)
		first, _ := winningTicket(t, 1)
		second, _ := winningTicket(t, 2)
		second.PayerAddress = first.PayerAddress
		// The batch exceeds collateral after both inserts; nothing may remain.
		err := s.StoreWinningTickets(t.Context(), first.PayerAddress, first.RecipientRandHash, big.NewInt(1), []*pm.SignedTicket{first, second}, pm.ChainSnapshot{})
		require.ErrorIs(t, err, pm.ErrPayerUnavailable)
		var rows int
		require.NoError(t, s.db.QueryRow("SELECT count(*) FROM winning_tickets").Scan(&rows))
		require.Zero(t, rows)
		require.NoError(t, s.db.QueryRow("SELECT count(*) FROM ticket_state").Scan(&rows))
		require.Zero(t, rows)
		liability, err := s.UnsettledLiability(t.Context(), first.PayerAddress)
		require.NoError(t, err)
		require.Zero(t, liability.Sign())
	})
}

func TestBackendRestartResumesPendingWork(t *testing.T) {
	runBackends(t, func(t *testing.T) {
		f, s, ticket, c, key := newRecoveryTest(t, 31)
		prepared, err := c.PrepareRedemption(t.Context(), key, big.NewInt(1), redeemTicket(ticket))
		require.NoError(t, err)
		require.NoError(t, s.RecordPrepared(t.Context(), ticket, prepared))
		s = reopenRecoveryStore(t, s)
		bindRecoveryStore(t, s, c)
		require.Equal(t, "prepared", ticketPhase(t, s, ticket.Sig))
		// The restarted worker sends the exact prepared bytes, then completes.
		tickRecovery(t, s, c, key)
		require.Len(t, f.Transactions(), 1)
		require.Equal(t, prepared.Hash, f.Transactions()[0].Hash())
		f.Mine(f.Transactions()[0], true)
		require.Empty(t, ReconcileSubmitted(t.Context(), s, c))
		require.Equal(t, "confirmed", ticketPhase(t, s, ticket.Sig))
		count, err := s.WinningTicketCount(ticket.PayerAddress)
		require.NoError(t, err)
		require.Zero(t, count)
	})
}

func TestPostgresOwnershipAndConnectionInterruption(t *testing.T) {
	d := newPostgresTestDatabase(t)
	s, err := openTestDatabase(t, d)
	require.NoError(t, err)
	defer s.Close()
	b := Binding{ChainID: "1", Controller: common.HexToAddress("0x1000"), Role: "redeemer", Destination: "chain"}
	require.NoError(t, s.Bind(t.Context(), b, DefaultLimits, nil))

	// A second process may read, but every write is fenced while s owns the database.
	other, err := openTestDatabase(t, d)
	require.NoError(t, err)
	_, err = other.Redemptions()
	require.NoError(t, err)
	require.ErrorIs(t, other.Bind(t.Context(), b, DefaultLimits, nil), ErrDatabaseOwned)
	first, _ := winningTicket(t, 1)
	require.ErrorIs(t, other.Admit(t.Context(), first), ErrDatabaseOwned)

	// Terminating every session, including the ownership session, models a
	// server restart or network partition. The owner reacquires the lock.
	require.NoError(t, other.Close())
	test.TerminatePostgresSessions(t, d.PostgresURL)
	var admitErr error
	for attempt := range 3 {
		if admitErr = s.Admit(t.Context(), first); admitErr == nil {
			t.Logf("admitted after interruption on attempt %d", attempt+1)
			break
		}
		t.Logf("attempt %d after interruption: %v", attempt+1, admitErr)
	}
	require.NoError(t, admitErr, "the owner recovers after its connections are interrupted")
	require.NoError(t, s.CheckStorage(t.Context()))
	count, err := s.WinningTicketCount(first.PayerAddress)
	require.NoError(t, err)
	require.Equal(t, 1, count)

	// Once the owner closes, another process takes over the durable queue.
	require.NoError(t, s.Close())
	next, err := openTestDatabase(t, d)
	require.NoError(t, err)
	defer next.Close()
	require.NoError(t, next.Bind(t.Context(), b, DefaultLimits, nil))
	count, err = next.WinningTicketCount(first.PayerAddress)
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestDatabaseConfiguration(t *testing.T) {
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		require.NoError(t, err)
		return u
	}
	key := filepath.Join(t.TempDir(), "client.key")
	require.NoError(t, os.WriteFile(key, []byte("key"), 0600))
	loose := filepath.Join(t.TempDir(), "loose.key")
	require.NoError(t, os.WriteFile(loose, []byte("key"), 0644))
	for _, tc := range []struct {
		name     string
		database Database
		err      string
		sslmode  string
	}{
		{name: "default sqlite", database: Database{Path: "tickets.sqlite"}},
		{name: "sqlite requires path", database: Database{Backend: "sqlite"}, err: "path is required"},
		{name: "unknown backend", database: Database{Backend: "mysql", Path: "x"}, err: "unsupported"},
		{name: "postgres requires URL", database: Database{Backend: "postgres"}, err: "requires a connection URL"},
		{name: "scheme", database: Database{Backend: "postgres", PostgresURL: parse("mysql://db/x")}, err: "postgres:// scheme"},
		{name: "loopback may be plaintext", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u:p@127.0.0.1/x?sslmode=disable")}, sslmode: "disable"},
		{name: "unix socket", database: Database{Backend: "postgres", PostgresURL: parse("postgres:///x?host=/var/run/postgresql")}},
		{name: "remote defaults to verify-full", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u:p@db.example/x")}, sslmode: "verify-full"},
		{name: "remote rejects unverified", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u:p@db.example/x?sslmode=require")}, err: "verify-full or verify-ca"},
		{name: "remote insecure opt-in", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u:p@db.example/x?sslmode=disable"), PostgresInsecure: true}, sslmode: "disable"},
		{name: "mixed hosts are remote", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u:p@127.0.0.1,db.example/x?sslmode=prefer")}, err: "verify-full or verify-ca"},
		{name: "unknown sslmode", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u:p@127.0.0.1/x?sslmode=sometimes")}, err: "unsupported"},
		{name: "owner-only client key", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u@127.0.0.1/x?sslkey=" + loose)}, err: "owner-only"},
		{name: "client key", database: Database{Backend: "postgres", PostgresURL: parse("postgres://u@127.0.0.1/x?sslmode=disable&sslkey=" + key)}, sslmode: "disable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.database.Validate()
			if tc.err != "" {
				require.ErrorContains(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			if tc.database.Backend == BackendPostgres {
				config, err := tc.database.postgresConfig()
				require.NoError(t, err)
				require.Equal(t, "livepeer-redeemer", config.RuntimeParams["application_name"])
				if tc.sslmode == "verify-full" {
					require.NotNil(t, config.TLSConfig)
					require.False(t, config.TLSConfig.InsecureSkipVerify)
					require.Empty(t, config.Fallbacks, "verified TLS never falls back to plaintext")
				}
				if tc.sslmode == "disable" {
					require.Nil(t, config.TLSConfig)
				}
			}
		})
	}
	described := Database{Backend: "postgres", PostgresURL: parse("postgres://u:secret@db.example:5432/x?sslmode=verify-full&password=other")}.Describe()
	require.Equal(t, "postgres://u@db.example:5432/x", described)
	require.Equal(t, "tickets.sqlite", Database{Path: "tickets.sqlite"}.Describe())
}

func TestPostgresConnectionFailureRedactsCredentials(t *testing.T) {
	d := Database{Backend: BackendPostgres, PostgresURL: &url.URL{Scheme: "postgres", User: url.UserPassword("redeemer", "s3cret-value"), Host: "127.0.0.1:1", Path: "/x", RawQuery: "connect_timeout=1"}}
	_, err := d.Open(t.Context(), true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "connect to redemption PostgreSQL")
	require.NotContains(t, err.Error(), "s3cret-value")
}
