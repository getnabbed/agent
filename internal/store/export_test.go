package store

import (
	"database/sql"
	"path/filepath"
)

func sqlOpenRO(dir string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, FileName))+"?mode=ro")
}
