package core

import (
	"context"
	"sync"
)

// Target names one database to back up. Several Targets can point at the
// same server (same host/user/password, different DBName) to back up
// multiple databases off one instance, or at entirely different servers —
// Manager doesn't care which.
type Target struct {
	Conn ConnectionInfo
}

// BackupAll runs Backup for every target through p, up to maxConcurrency
// at a time (values <= 0 are treated as 1, i.e. sequential). Results are
// returned in the same order as targets regardless of completion order.
// A per-target failure does not stop the others; check BackupResult.OK()
// on each entry.
func BackupAll(ctx context.Context, p Provider, targets []Target, opts BackupOptions, maxConcurrency int) []BackupResult {
	return BackupAllWithProgress(ctx, p, targets, opts, maxConcurrency, nil)
}

// BackupAllWithProgress is BackupAll plus an onResult callback, invoked
// as each target finishes (before the result is stored), so a caller can
// render incremental progress instead of waiting for the whole batch.
// onResult may be nil; when set, it's called concurrently from multiple
// goroutines when maxConcurrency > 1, so it must be safe for that.
func BackupAllWithProgress(ctx context.Context, p Provider, targets []Target, opts BackupOptions, maxConcurrency int, onResult func(index int, res BackupResult)) []BackupResult {
	results := make([]BackupResult, len(targets))

	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	sem := make(chan struct{}, maxConcurrency)

	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t Target) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				res := BackupResult{DBName: t.Conn.DBName, Err: ctx.Err()}
				results[i] = res
				if onResult != nil {
					onResult(i, res)
				}
				return
			}
			defer func() { <-sem }()

			res, err := p.Backup(ctx, t.Conn, opts)
			if err != nil && res.Err == nil {
				res.Err = err
			}
			if res.DBName == "" {
				res.DBName = t.Conn.DBName
			}
			results[i] = res
			if onResult != nil {
				onResult(i, res)
			}
		}(i, t)
	}
	wg.Wait()
	return results
}

// RestoreTarget pairs a connection with the restore options for one
// database. Unlike Target/BackupOptions, options are per-target here
// because each restore needs its own source file (InputPath) — there's no
// shared "just pick an output name" convention to fall back on the way
// there is for backups.
type RestoreTarget struct {
	Conn ConnectionInfo
	Opts RestoreOptions
}

// RestoreAll runs Restore for every target through p, up to
// maxConcurrency at a time (values <= 0 are treated as 1). Results are
// returned in the same order as targets regardless of completion order.
// A per-target failure does not stop the others; check RestoreResult.OK()
// on each entry.
func RestoreAll(ctx context.Context, p Provider, targets []RestoreTarget, maxConcurrency int) []RestoreResult {
	return RestoreAllWithProgress(ctx, p, targets, maxConcurrency, nil)
}

// RestoreAllWithProgress is RestoreAll plus an onResult callback, invoked
// as each target finishes (before the result is stored), so a caller can
// render incremental progress instead of waiting for the whole batch.
// onResult may be nil; when set, it's called concurrently from multiple
// goroutines when maxConcurrency > 1, so it must be safe for that.
func RestoreAllWithProgress(ctx context.Context, p Provider, targets []RestoreTarget, maxConcurrency int, onResult func(index int, res RestoreResult)) []RestoreResult {
	results := make([]RestoreResult, len(targets))

	if maxConcurrency <= 0 {
		maxConcurrency = 1
	}
	sem := make(chan struct{}, maxConcurrency)

	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func(i int, t RestoreTarget) {
			defer wg.Done()

			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				res := RestoreResult{DBName: t.Conn.DBName, Err: ctx.Err()}
				results[i] = res
				if onResult != nil {
					onResult(i, res)
				}
				return
			}
			defer func() { <-sem }()

			res, err := p.Restore(ctx, t.Conn, t.Opts)
			if err != nil && res.Err == nil {
				res.Err = err
			}
			if res.DBName == "" {
				res.DBName = t.Conn.DBName
			}
			results[i] = res
			if onResult != nil {
				onResult(i, res)
			}
		}(i, t)
	}
	wg.Wait()
	return results
}
