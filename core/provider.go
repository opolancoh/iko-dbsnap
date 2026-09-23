package core

import "context"

// Provider is implemented once per database engine (postgres, mysql,
// sqlserver, ...). All engine-specific logic — which binaries to shell
// out to, flag syntax, default ports, file layout — must stay behind this
// interface so the rest of the tool never needs to know which engine it's
// talking to.
type Provider interface {
	// Name identifies the provider, e.g. "postgres". Used as the registry
	// key and in CLI flags/logs.
	Name() string

	// Backup dumps the single database described by conn into
	// opts.OutputDir and reports where it landed.
	Backup(ctx context.Context, conn ConnectionInfo, opts BackupOptions) (BackupResult, error)

	// Restore creates the database named by conn.DBName and loads a
	// backup artifact (opts.InputPath, as produced by Backup) into it. It
	// must fail without touching anything if that database already
	// exists — dbsnap only ever restores into a database it just created
	// itself, never into an existing one (empty or not). See PlanRestore
	// for the friendlier pre-flight version of that check.
	Restore(ctx context.Context, conn ConnectionInfo, opts RestoreOptions) (RestoreResult, error)
}
