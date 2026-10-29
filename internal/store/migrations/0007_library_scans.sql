-- Library scans requested by RescanLibrary and not yet reported in full.
-- A scan interrupted by a restart starts over from here with the same id.
CREATE TABLE library_scans (
    root_id    TEXT PRIMARY KEY,
    scan_id    TEXT NOT NULL,
    created_at INTEGER NOT NULL
);
