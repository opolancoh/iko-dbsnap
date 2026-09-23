// Package core holds the provider-agnostic building blocks for dbsnap:
// connection/option/result types, the Provider interface, a registry
// providers plug into, and a concurrent runner. Nothing in this package
// may import a specific database provider (postgres, mysql, ...) — that
// dependency only ever flows the other way.
package core

import "time"

// ConnectionInfo holds the connection parameters needed to reach a single
// database. Fields that don't apply to a given provider can be left zero.
// Provider-specific extras (e.g. Postgres "sslmode", MySQL "tls") go in
// Params instead of growing this struct.
type ConnectionInfo struct {
	Host     string
	Port     int // 0 means "use the provider's default port"
	User     string
	Password string
	DBName   string

	Params map[string]string
}

// Param returns Params[key], or def if it's unset/empty.
func (c ConnectionInfo) Param(key, def string) string {
	if v, ok := c.Params[key]; ok && v != "" {
		return v
	}
	return def
}

// BackupOptions configures a single backup run. OutputDir is where the
// resulting artifact is written. Format is provider-specific (e.g.
// Postgres supports "custom"/"plain"/"directory"/"tar") — each provider
// documents its own valid values and picks a sane default when empty.
type BackupOptions struct {
	OutputDir string
	Format    string
	Params    map[string]string

	// OnProgress, if set, is called by providers that can report
	// fine-grained progress as the backup runs (e.g. "now dumping table
	// X"). Providers that can't report progress simply never call it —
	// callers must not assume it will fire at all, let alone for every
	// table. It may be called concurrently from multiple goroutines when
	// several targets are running at once (see BackupAllWithProgress), so
	// it must be safe for concurrent use.
	OnProgress func(ProgressEvent)
}

// Param returns Params[key], or def if it's unset/empty.
func (o BackupOptions) Param(key, def string) string {
	if v, ok := o.Params[key]; ok && v != "" {
		return v
	}
	return def
}

// BackupResult reports the outcome of backing up a single database.
type BackupResult struct {
	DBName    string
	FilePath  string
	SizeBytes int64
	StartedAt time.Time
	Duration  time.Duration
	Err       error
}

// OK reports whether the backup completed without error.
func (r BackupResult) OK() bool { return r.Err == nil }

// ProgressEvent is one unit of progress reported by a provider during a
// backup, e.g. "started dumping this table". It's intentionally coarse —
// providers generally can't report exact rows-written-so-far without
// abandoning their underlying dump tool, so this is a "what's happening
// now" signal for a UI, not a verified completion count. Table/Schema may
// be empty if a provider only reports at database granularity.
type ProgressEvent struct {
	DBName string
	Schema string
	Table  string
}

// RestoreOptions configures restoring a single database from a backup
// artifact previously produced by Backup.
type RestoreOptions struct {
	// InputPath is the backup file (or, for formats like Postgres's
	// "directory", the backup directory) to restore from.
	InputPath string

	// Format is provider-specific; empty means "infer from InputPath".
	Format string

	// NoOwner skips restoring ownership/ACL metadata — useful when
	// restoring into a database owned by a different role than the one
	// that produced the backup.
	NoOwner bool

	Params map[string]string

	// OnProgress, if set, is called by providers that can report
	// fine-grained progress as the restore runs. Same caveats as
	// BackupOptions.OnProgress: not guaranteed to fire, and must be safe
	// for concurrent use.
	OnProgress func(ProgressEvent)
}

// Param returns Params[key], or def if it's unset/empty.
func (o RestoreOptions) Param(key, def string) string {
	if v, ok := o.Params[key]; ok && v != "" {
		return v
	}
	return def
}

// RestoreResult reports the outcome of restoring a single database.
type RestoreResult struct {
	DBName    string
	StartedAt time.Time
	Duration  time.Duration
	Err       error

	// Warnings holds errors the provider's restore tool reported but
	// skipped past rather than aborting on (e.g. pg_restore's "errors
	// ignored on restore"). The restore still counts as OK — typically
	// these are settings the target server's version doesn't recognize —
	// but callers must surface them, since they can also mean an object
	// was not restored.
	Warnings []string
}

// OK reports whether the restore completed without error.
func (r RestoreResult) OK() bool { return r.Err == nil }
