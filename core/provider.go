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

	// Restore loads a backup artifact (opts.InputPath, as produced by
	// Backup) into the database described by conn. If that database
	// doesn't exist, opts.Create controls whether it's created first (see
	// core.DecideRestorePlan for when that's safe to set). Restoring into
	// an existing, non-empty database may fail on conflicting objects —
	// there's deliberately no option to drop existing objects first.
	Restore(ctx context.Context, conn ConnectionInfo, opts RestoreOptions) (RestoreResult, error)
}
