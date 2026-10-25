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

CREATE TABLE IF NOT EXISTS models (
    atespace text        NOT NULL,
    name     text        NOT NULL,
    data     jsonb       NOT NULL,
    saved_at timestamptz NOT NULL,
    PRIMARY KEY (atespace, name)
);
CREATE INDEX IF NOT EXISTS models_atespace_saved_at_idx ON models (atespace, saved_at DESC, name);

-- Leases backing the distributed Locker. A row whose expires_at has passed is
-- free and may be taken over by the next caller.
CREATE TABLE IF NOT EXISTS locks (
    key        text        PRIMARY KEY,
    token      text        NOT NULL,
    expires_at timestamptz NOT NULL
);
