-- Agent notices (design 0008 §2): the state of each condition, keyed by code
-- and subject, so a restart doesn't resend an open notice. open is the
-- condition's current state and sent_open the state last queued in the
-- outbox; they differ while the rate limit holds a change back.
CREATE TABLE notices (
    code       TEXT NOT NULL,
    subject_id TEXT NOT NULL,
    open       INTEGER NOT NULL,
    sent_open  INTEGER NOT NULL DEFAULT 0,
    severity   INTEGER NOT NULL,
    message    TEXT NOT NULL,
    attrs      TEXT NOT NULL DEFAULT '{}',
    updated_at INTEGER NOT NULL,
    sent_at    INTEGER,
    PRIMARY KEY (code, subject_id)
);
