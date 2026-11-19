# PostgreSQL storage for the standalone redeemer

A [standalone redeemer](redeemer.md) can keep its winning-ticket queue and
redemption transactions in PostgreSQL instead of SQLite. SQLite remains the
default; existing configurations are unaffected. Orchestrators and signers
still use SQLite (see [remaining stores](#remaining-stores)).

Use PostgreSQL when the redeemer runs where a local persistent disk is
inconvenient, or when you already operate managed PostgreSQL with backups and
monitoring. PostgreSQL 13 or later is supported; tests run against 16.

## Configure

```toml
RedeemerDBBackend = "postgres"
RedeemerPostgresURLFile = "/run/secrets/livepeer-redeemer-postgres-url"
# RedeemerPostgresMaxConns = 8
# RedeemerPostgresInsecure = false
```

The URL file is an owner-only file containing one `postgres://` URL, for example
`postgres://redeemer:PASSWORD@db.internal:5432/redeemer?sslmode=verify-full&sslrootcert=/etc/livepeer/pg-ca.pem`.
It is a credential: like the RPC URL it is read only from the file or from
`LIVEPEER_REDEEMER_REDEEMER_POSTGRES_URL`, never printed, and redacted from
connection errors. `--print-config` shows only scheme, user, host and database.

| Setting | Default | Meaning |
| --- | --- | --- |
| `RedeemerDBBackend` | `sqlite` | `sqlite` or `postgres`. With `postgres`, `RedeemerDB` is ignored. |
| `RedeemerPostgresURL` / `RedeemerPostgresURLFile` | none | Connection URL. Required for `postgres`. |
| `RedeemerPostgresInsecure` | `false` | Permit `sslmode` `disable`, `allow`, `prefer` or `require` to non-loopback hosts. |
| `RedeemerPostgresMaxConns` | `8` | Pool size, minimum 2. One connection is held for [ownership](#process-ownership). |

TLS policy:

- Loopback addresses and Unix sockets (`host=/var/run/postgresql`) may use any
  `sslmode`.
- For any other host, a URL without `sslmode` is upgraded to `verify-full`.
  Only `verify-full` and `verify-ca` are accepted unless
  `RedeemerPostgresInsecure = true`. Prefer `verify-full`.
- Client certificates use the standard `sslcert` and `sslkey` URL parameters;
  `sslkey` must be an owner-only regular file. Passwords can also come from
  a `passfile` URL parameter or `PGPASSFILE`; keep that file owner-only.

The redeemer sets `application_name=livepeer-redeemer` and a 10 second
`connect_timeout` unless the URL sets them. Startup fails if the server is
unreachable; let the supervisor restart it.

## Provision

Create a dedicated role and database, owned by that role so migrations can
create tables (PostgreSQL 15 and later no longer grant `CREATE` on `public`):

```sql
CREATE ROLE livepeer_redeemer LOGIN PASSWORD '...';
CREATE DATABASE livepeer_redeemer OWNER livepeer_redeemer;
```

Use one database per redeemer. Do not share it with other applications or put
two redeemers in different schemas of one database: process ownership is
per database. Connect directly or through a pooler in **session** mode;
transaction-mode pooling (for example PgBouncer `pool_mode=transaction`) breaks
the session advisory lock that fences other processes.

## Migrations

The redeemer applies pending migrations at startup, as with SQLite. To inspect
or apply them explicitly, pass the same configuration:

```sh
livepeer redeemer migrate status --config /etc/livepeer/redeemer.toml
livepeer redeemer migrate up --config /etc/livepeer/redeemer.toml
```

`--redeemer-db-backend postgres --redeemer-postgres-url-file FILE` select the
database without a config file. `migrate down` destroys data.

PostgreSQL migrations live in `redemption/postgres/migrations` and are versioned
separately from the SQLite catalog in `redemption/migrations`. PostgreSQL
version 1 creates the schema SQLite reaches at version 5. History is recorded
in the same `migrations` table with SHA-256 checksums, so a changed or unknown
migration blocks startup exactly as it does on SQLite. Future schema changes
add a migration to each catalog.

## Back up and restore

The queue holds ticket signatures and redemption randomness: protect dumps
like the SQLite file. Take a consistent logical backup while the redeemer runs:

```sh
pg_dump --format=custom --file=redeemer.dump "$(cat /run/secrets/livepeer-redeemer-postgres-url)"
```

Or rely on your provider's snapshots or point-in-time recovery. To restore,
stop the redeemer, restore into an **empty** database, then start it:

```sh
createdb -O livepeer_redeemer livepeer_redeemer_restored
pg_restore --no-owner --role=livepeer_redeemer --dbname=livepeer_redeemer_restored redeemer.dump
```

The [recovery guidance](payment-recovery.md) applies unchanged: restoring an
older backup can lose transaction identities, so prefer the latest state.
Advisory locks are not part of a dump.

## Switch an existing SQLite redeemer to PostgreSQL

There is no automatic copy of SQLite state. Cut over by draining, so every
pending ticket and every signed transaction completes in the SQLite database
that recorded it, and new tickets wait in the orchestrators.

1. Provision the PostgreSQL database and run
   `livepeer redeemer migrate up` with the new settings to check connectivity.
2. Stop new submissions to the redeemer, for example by having your proxy
   answer `POST /tickets` with `503 Service Unavailable`. Orchestrators keep
   undelivered tickets in their local queues and retry; size their
   queues (`MaxPendingTickets`) for the cutover window.
3. Keep the SQLite redeemer running until it is drained: the
   `redemption_queue_tickets` and `redemption_unresolved_transactions` metrics
   are zero for every state and phase, and `livepeer redeemer redemptions`
   shows only terminal outcomes. Tickets whose parameters have not expired
   wait for their expiration block; reverted or rejected tickets need
   operator attention before they settle or expire.
4. Stop the SQLite redeemer and archive its database (see
   [backups](payment-recovery.md#back-up-sqlite)). Keep it: it is the audit
   record for completed redemptions.
5. Start the redeemer with `RedeemerDBBackend = "postgres"`, the same wallet,
   chain and Controller. Once ready, re-enable submissions; orchestrators
   redeliver the tickets they held.

Because the wallet's signed nonces were all finalized during the drain, the new
store starts from the chain's nonce. Redelivered tickets that were already
redeemed are detected onchain and recorded as used. Switching back to SQLite
follows the same procedure in reverse.

Do not start the PostgreSQL redeemer while the SQLite one still has signed,
unresolved transactions: the new store cannot see those nonces.

## Design

### One set of queries

Store queries are written once in SQLite syntax with `?` and `?N`
placeholders. A thin wrapper (`redemption/sqldb.go`) rewrites placeholders to
`$N` for PostgreSQL (`internal/sqldialect`). SQLite-only constructs were
replaced by portable SQL with the same results: scalar `MAX()` over 0/1 flags
became `CASE`, boolean sums became `CASE` sums, `IS NOT` became
`IS DISTINCT FROM`, derived tables gained aliases and an upsert condition is
qualified. Only the redemption listing's hex encoding differs per backend.
`redemption_transactions` has an identity column named `rowid` on PostgreSQL,
so ordering and the bounded recovery cursors use the same text on both.

The migration runner (`nodeconfig/migrations`) detects the backend for
history-table creation and introspection (`pg_catalog` instead of
`sqlite_master`) and chooses the `postgres/migrations` catalog.

### Isolation

SQLite serializes all redeemer work on a single connection. PostgreSQL
reproduces that order for writes:

- Every write transaction, and every single-statement write, first takes a
  transaction-scoped advisory lock (`pg_advisory_xact_lock`) shared by all
  writers. Writers therefore run one at a time at `READ COMMITTED`, each
  statement seeing every earlier commit, so ticket admission, deduplication,
  capacity limits, liability checks and transaction state changes stay
  atomic without serialization-failure retries.
- Read-only multi-statement reports (metrics, liability reads) use
  `REPEATABLE READ` snapshots. Single-statement reads use `READ COMMITTED`,
  as they could interleave with writes on SQLite too.

Throughput is bounded by one writer, as on SQLite; the redeemer's write rate is
far below that bound.

### Process ownership

The first write takes a session-level advisory lock on a dedicated
connection and keeps it for the life of the process. Every later write
transaction verifies, while holding the writer lock, that this session (by
process id and backend start time) still holds ownership. Consequently:

- A second redeemer or a `redemptions --retry-transaction` command against a
  running redeemer's database fails with "redemption database is owned by
  another process". Read-only inspection (`redemptions`, `migrate status`)
  works.
- If the owner session is lost (server restart, network partition), the next
  write reacquires ownership on a new connection unless another process has
  taken it, in which case this process's writes are refused. The readiness
  pass performs such a write check every few seconds, so `/readyz` reports a
  lost or contested database promptly.
- An interrupted transaction rolls back. A commit whose acknowledgment is lost
  is reported as an error; the operation is idempotent on retry (ticket
  admission deduplicates by hash, and transaction records are keyed by hash).

## Testing

PostgreSQL integration tests run when `LIVEPEER_TEST_POSTGRES_URL` names a
server and role allowed to `CREATE DATABASE`; each test uses a fresh database.

```sh
LIVEPEER_TEST_POSTGRES_URL=postgres://redeemer:PASSWORD@127.0.0.1:5432/postgres \
  go test ./redemption/... ./redeemer/...
```

Without the variable they are skipped. The redemption store's lifecycle tests
run on SQLite normally and again on PostgreSQL in
`TestPostgresSharedStoreBehavior`; backend-parametrized tests cover fresh
initialization, concurrent duplicate and capacity-limited admission, rollback,
restart recovery, ownership and connection interruption.

## Remaining stores

Follow-up work to extend PostgreSQL beyond the standalone redeemer:

- **Orchestrator payments** (`payments.sqlite`, `orchestrator/redemption.go`):
  embeds the same `RedeemerDB`, so the store already works; it needs
  configuration, a decision on process ownership for the orchestrator's
  payment admission path (latency under the global writer lock), and cutover
  documentation. Orchestrator storage metrics read the SQLite file size.
- **Signer Kafka outbox** (`events.sqlite`, `signer/outbox.go`): a separate
  schema and queries; needs a PostgreSQL catalog, placeholder rebinding and an
  ownership model for one outbox per signer replica.
- **Data migration tool**: copy a drained or live SQLite redemption database
  into PostgreSQL, preserving ordering and transaction identities, to avoid
  the drain-based cutover.
- **Storage metrics**: `redemption_storage_bytes` is reported only for SQLite;
  add `pg_database_size` or document provider metrics.
- **Fencing exit**: a redeemer that loses ownership to another process stays
  running but unready and refuses writes; exiting would make supervisors
  surface the conflict faster.
