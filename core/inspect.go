package core

import "context"

// TableInspection describes one table as seen during pre-flight discovery.
type TableInspection struct {
	Schema   string
	Name     string
	RowCount int64
}

// SchemaInspection groups the tables found under one schema.
type SchemaInspection struct {
	Name   string
	Tables []TableInspection
}

// DatabaseInspection is the full pre-flight picture of a single database:
// every schema and table in it, with row counts, as of the moment it was
// inspected.
type DatabaseInspection struct {
	DBName  string
	Schemas []SchemaInspection
}

// TableCount returns the total number of tables across all schemas.
func (d DatabaseInspection) TableCount() int {
	n := 0
	for _, s := range d.Schemas {
		n += len(s.Tables)
	}
	return n
}

// RowCount returns the total row count across all tables in all schemas.
func (d DatabaseInspection) RowCount() int64 {
	var n int64
	for _, s := range d.Schemas {
		for _, t := range s.Tables {
			n += t.RowCount
		}
	}
	return n
}

// Inspector is implemented by providers that can introspect a server
// before backing it up: verify connectivity, enumerate which databases
// actually exist, and report a database's schema/table/row-count shape.
// It's optional — a Provider that doesn't support discovery simply
// doesn't implement it, so callers should type-assert rather than require
// it.
type Inspector interface {
	// Ping verifies the server is reachable and the credentials are
	// valid. It does not require conn.DBName to exist — the point of
	// Ping is to fail fast on host/auth problems before we've even
	// established which requested databases are present.
	Ping(ctx context.Context, conn ConnectionInfo) error

	// ListDatabases returns the names of every database on the server
	// conn points at. conn.DBName is not used to filter the result.
	ListDatabases(ctx context.Context, conn ConnectionInfo) ([]string, error)

	// Inspect returns the schema/table/row-count structure of the single
	// database named by conn.DBName.
	Inspect(ctx context.Context, conn ConnectionInfo) (DatabaseInspection, error)
}

// DBCheck reports whether one requested database exists on the server.
type DBCheck struct {
	DBName string
	Found  bool
}

// CheckDatabases compares a list of requested database names against the
// server's actual list (as returned by Inspector.ListDatabases) and
// reports which requested ones exist, in the same order as requested.
func CheckDatabases(requested, existing []string) []DBCheck {
	exists := make(map[string]bool, len(existing))
	for _, n := range existing {
		exists[n] = true
	}
	out := make([]DBCheck, len(requested))
	for i, n := range requested {
		out[i] = DBCheck{DBName: n, Found: exists[n]}
	}
	return out
}

// ArchiveInfo is what can be learned about a backup file without
// connecting to any server — just by reading the file itself.
type ArchiveInfo struct {
	// DBName is the database name recorded inside the archive at backup
	// time, if the format captures one. Empty when the format doesn't
	// record it (e.g. a plain SQL dump made without pg_dump -C).
	DBName string
	Format string
}

// ArchiveInspector is implemented by providers that can read metadata out
// of a backup file before restoring it. Like Inspector, it's optional.
type ArchiveInspector interface {
	// InspectArchive reads what it can from the backup file at path
	// without restoring it or connecting to any server.
	InspectArchive(ctx context.Context, path string) (ArchiveInfo, error)
}
