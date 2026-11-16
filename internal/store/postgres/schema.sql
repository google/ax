-- Resources are stored as protojson documents. Only the columns needed to
-- address and order them are broken out, so adding fields to the API does not
-- require a schema change. saved_at changes on every Save but not on status
-- updates; lists are ordered by it, newest first.

CREATE TABLE IF NOT EXISTS tasks (
    atespace text        NOT NULL,
    name     text        NOT NULL,
    data     jsonb       NOT NULL,
    saved_at timestamptz NOT NULL,
    PRIMARY KEY (atespace, name)
);
CREATE INDEX IF NOT EXISTS tasks_saved_at_idx ON tasks (saved_at DESC, atespace, name);
CREATE INDEX IF NOT EXISTS tasks_atespace_saved_at_idx ON tasks (atespace, saved_at DESC, name);

CREATE TABLE IF NOT EXISTS workspaces (
    atespace text        NOT NULL,
    name     text        NOT NULL,
    data     jsonb       NOT NULL,
    saved_at timestamptz NOT NULL,
    PRIMARY KEY (atespace, name)
);
CREATE INDEX IF NOT EXISTS workspaces_atespace_saved_at_idx ON workspaces (atespace, saved_at DESC, name);

CREATE TABLE IF NOT EXISTS gateways (
    atespace text        NOT NULL,
    name     text        NOT NULL,
    data     jsonb       NOT NULL,
    saved_at timestamptz NOT NULL,
    PRIMARY KEY (atespace, name)
);
CREATE INDEX IF NOT EXISTS gateways_atespace_saved_at_idx ON gateways (atespace, saved_at DESC, name);

CREATE TABLE IF NOT EXISTS models (
    atespace text        NOT NULL,
    name     text        NOT NULL,
    data     jsonb       NOT NULL,
    saved_at timestamptz NOT NULL,
    PRIMARY KEY (atespace, name)
);
CREATE INDEX IF NOT EXISTS models_atespace_saved_at_idx ON models (atespace, saved_at DESC, name);

-- Leases backing the distributed Locker. Multiple shared ('S') leases may
-- coexist for the same key, whereas an exclusive ('X') lease requires no other
-- active leases for that key. Rows whose expires_at has passed are treated as
-- free and cleaned up on subsequent acquisitions.
CREATE TABLE IF NOT EXISTS locks (
    key        text        NOT NULL,
    token      text        NOT NULL,
    mode       text        NOT NULL DEFAULT 'X' CHECK (mode IN ('S', 'X')),
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (key, token)
);
ALTER TABLE locks ADD COLUMN IF NOT EXISTS mode text NOT NULL DEFAULT 'X' CHECK (mode IN ('S', 'X'));
-- Pre-release in-place upgrade from single-column PRIMARY KEY (key). Stop old
-- pre-RLock ax-server replicas before upgrading so they do not run legacy
-- `INSERT ... ON CONFLICT (key)` after locks_pkey moves to (key, token).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM pg_index i
        JOIN pg_class c ON c.oid = i.indrelid
        WHERE c.relname = 'locks' AND i.indisprimary AND i.indnatts = 1
    ) THEN
        ALTER TABLE locks DROP CONSTRAINT locks_pkey;
        ALTER TABLE locks ADD PRIMARY KEY (key, token);
    END IF;
END $$;
