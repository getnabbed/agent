-- The state a download failed in, so a retry of a failed import or
-- assembly resumes there with the files already on disk.
ALTER TABLE downloads ADD COLUMN failed_from INTEGER NOT NULL DEFAULT 0;
