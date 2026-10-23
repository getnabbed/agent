-- Identity binding and other single values.
CREATE TABLE meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
) STRICT;

-- Every command the agent has answered, with the exact result sent. A
-- redelivered command id replays the stored result instead of re-applying.
-- seq orders application; the newest row is Hello.last_command_id.
CREATE TABLE applied_commands (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT    NOT NULL UNIQUE,
    kind       TEXT    NOT NULL,
    outcome    INTEGER NOT NULL,
    result     BLOB    NOT NULL, -- serialised nabbed.agent.v2.CommandResult
    applied_at INTEGER NOT NULL  -- unix ms
) STRICT;

-- Events waiting for EventAck. Written in the same transaction as the state
-- change they describe; drained in seq order; deleted on ack.
CREATE TABLE outbox (
    seq        INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT    NOT NULL UNIQUE, -- Header.id, the cloud's dedup key
    kind       TEXT    NOT NULL,
    payload    BLOB    NOT NULL,        -- serialised nabbed.agent.v2.Event
    created_at INTEGER NOT NULL         -- unix ms
) STRICT;
