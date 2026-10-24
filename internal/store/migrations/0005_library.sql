-- Library roots from the control plane. path is as the cloud sent it; it is
-- checked against the local allowed roots again whenever it is used, since
-- a symlink may have changed.
CREATE TABLE library_roots (
    id              TEXT PRIMARY KEY,
    kind            INTEGER NOT NULL,
    path            TEXT NOT NULL,
    folder_template TEXT NOT NULL,
    file_template   TEXT NOT NULL,
    specials_folder TEXT NOT NULL,
    updated_at      INTEGER NOT NULL
);

-- The single import policy (SetImportPolicy), serialised.
CREATE TABLE import_policy (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    spec       BLOB NOT NULL,
    updated_at INTEGER NOT NULL
);
