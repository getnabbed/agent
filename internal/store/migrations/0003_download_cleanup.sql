-- Set once a cancelled download's work directory has been removed, so the
-- scheduler never rescans it.
ALTER TABLE downloads ADD COLUMN data_removed INTEGER NOT NULL DEFAULT 0;

CREATE INDEX downloads_cleanup ON downloads (state, data_removed);
