-- One row per download the cloud has started. state is a
-- nabbed.agent.v2.DownloadState; every change is made by a compare-and-set
-- UPDATE in the same transaction as its DownloadUpdate outbox event.
CREATE TABLE downloads (
    id                TEXT    PRIMARY KEY,
    acquisition_id    TEXT    NOT NULL,
    state             INTEGER NOT NULL,
    protocol          INTEGER NOT NULL,
    spec              BLOB    NOT NULL,            -- serialised nabbed.agent.v2.Download
    priority          INTEGER NOT NULL DEFAULT 0,
    source_id         TEXT    NOT NULL DEFAULT '',
    result            TEXT    NOT NULL DEFAULT '', -- engine.Result JSON, set when DOWNLOADING ends
    failure_class     INTEGER NOT NULL DEFAULT 0,
    failure_retryable INTEGER NOT NULL DEFAULT 0,
    failure_detail    TEXT    NOT NULL DEFAULT '',
    attempts          INTEGER NOT NULL DEFAULT 0,
    created_at        INTEGER NOT NULL,             -- unix ms
    updated_at        INTEGER NOT NULL              -- unix ms
) STRICT;

CREATE INDEX downloads_by_state ON downloads (state, priority DESC, created_at);
