package db

import (
	"database/sql"
	_ "embed"
	"fmt"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Open opens (creating if needed) the SQLite database at path and applies
// the schema.
func Open(path string) (*sql.DB, error) {
	conn, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One writer at a time; SQLite serialises writes anyway.
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec(schemaSQL); err != nil {
		conn.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	if err := migrate(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("migrating: %w", err)
	}
	return conn, nil
}

// added lists columns appended to existing tables after the first release.
// CREATE TABLE IF NOT EXISTS leaves an older file alone, so add any that are
// missing, in this order (it matches schema.sql, so SELECT * still lines up).
var added = []struct{ table, column, decl string }{
	{"measurements", "tilt_x", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "tilt_y", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "tilt_err", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "shadow_x_mm", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "shadow_y_mm", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "hub_x_mm", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "hub_y_mm", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "pupil_err_mm", "REAL NOT NULL DEFAULT 0"},
	{"measurements", "intra_high", "INTEGER NOT NULL DEFAULT 0"},
	{"measurements", "tilt_rough", "INTEGER NOT NULL DEFAULT 0"},
	{"measurements", "pupil_rough", "INTEGER NOT NULL DEFAULT 0"},
}

func migrate(conn *sql.DB) error {
	for _, a := range added {
		var n int
		err := conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, a.table, a.column).Scan(&n)
		if err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		if _, err := conn.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", a.table, a.column, a.decl)); err != nil {
			return err
		}
	}
	return nil
}
