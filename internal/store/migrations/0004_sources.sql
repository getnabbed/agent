-- Download sources from the control plane. sealed_config stays sealed to
-- the agent's X25519 key; it is opened only in memory. Rows keep their
-- rowid on upsert, so ORDER BY rowid is the order sources were first added.
CREATE TABLE sources (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    display_name  TEXT NOT NULL,
    sealed_config BLOB NOT NULL,
    sealed_to_key BLOB NOT NULL,
    enabled       INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL
);
