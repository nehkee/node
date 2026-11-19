-- UP
-- PostgreSQL schema equivalent to SQLite migrations 001-005. Versions are
-- numbered per backend; see docs/redeemer-postgres.md.
-- Text keeps SQLite's binary ordering where the store sorts or aggregates it.
CREATE TABLE winning_tickets (
 seq BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 payer_address TEXT NOT NULL, recipient TEXT NOT NULL,
 face_value BYTEA NOT NULL, win_prob BYTEA NOT NULL,
 ticket_nonce BIGINT NOT NULL, recipient_rand BYTEA NOT NULL,
 recipient_rand_hash TEXT NOT NULL, sig BYTEA NOT NULL UNIQUE,
 creation_round BIGINT NOT NULL, creation_round_block_hash TEXT NOT NULL,
 params_expiration_block TEXT NOT NULL,
 redeemed_at TEXT COLLATE "C", tx_hash TEXT,
 enqueued_at TEXT COLLATE "C",
 ticket_hash BYTEA
);
CREATE INDEX winning_tickets_epoch ON winning_tickets(recipient_rand_hash);
CREATE INDEX winning_tickets_liability ON winning_tickets(payer_address,creation_round) WHERE redeemed_at IS NULL;
CREATE INDEX winning_tickets_hash ON winning_tickets(ticket_hash);
CREATE INDEX winning_tickets_transaction ON winning_tickets(tx_hash);
CREATE TABLE redemption_attempts (
 sig BYTEA PRIMARY KEY, attempted_at TEXT NOT NULL, error TEXT,
 phase TEXT NOT NULL, raw_transaction BYTEA, redeemer_address TEXT, nonce TEXT COLLATE "C"
);
-- rowid stands in for SQLite's implicit row identifier: it orders transaction
-- history and bounds recovery sweeps, so store queries are shared verbatim.
CREATE TABLE redemption_transactions (
 rowid BIGINT GENERATED ALWAYS AS IDENTITY UNIQUE,
 hash TEXT PRIMARY KEY, sig BYTEA NOT NULL,
 root_hash TEXT NOT NULL, replaces_hash TEXT,
 raw_transaction BYTEA NOT NULL, redeemer_address TEXT NOT NULL, nonce TEXT COLLATE "C" NOT NULL,
 phase TEXT NOT NULL, error TEXT, prepared_at TEXT NOT NULL, broadcast_at TEXT,
 acknowledged INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX redemption_transactions_ticket ON redemption_transactions(sig);
CREATE INDEX redemption_transactions_root ON redemption_transactions(root_hash);
CREATE INDEX redemption_transactions_nonce ON redemption_transactions(redeemer_address,nonce);
CREATE INDEX redemption_transactions_acknowledged ON redemption_transactions(redeemer_address,length(nonce) DESC,nonce DESC) WHERE acknowledged=1;
CREATE TABLE collateral_watermark (
 id INTEGER PRIMARY KEY CHECK(id=1),
 number BIGINT NOT NULL CHECK(number>=0), hash TEXT NOT NULL
);
CREATE TABLE ticket_state (
 hash BYTEA PRIMARY KEY CHECK(length(hash)=32), sig BYTEA NOT NULL,
 exposed INTEGER NOT NULL DEFAULT 0,
 settled INTEGER NOT NULL DEFAULT 0, settlement TEXT NOT NULL DEFAULT '',
 delivery TEXT NOT NULL DEFAULT 'queued',
 failures BIGINT NOT NULL DEFAULT 0, next_attempt BIGINT NOT NULL DEFAULT 0, visited BIGINT NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX ticket_state_signature ON ticket_state(sig);
CREATE INDEX ticket_state_pending ON ticket_state(settled,next_attempt);
CREATE TABLE redemption_binding (
 id INTEGER PRIMARY KEY CHECK(id=1), chain_id TEXT NOT NULL,
 controller TEXT NOT NULL, role TEXT NOT NULL, destination TEXT NOT NULL
);
-- DOWN
DROP TABLE redemption_binding;
DROP TABLE ticket_state;
DROP TABLE collateral_watermark;
DROP TABLE redemption_transactions;
DROP TABLE redemption_attempts;
DROP TABLE winning_tickets;
