package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrNoDBName is returned by ResolveRestoreName (and PlanRestore) when no
// target database name was given and the backup doesn't record one
// either. The name is never guessed from the backup's file name.
var ErrNoDBName = errors.New("the backup does not record a database name and none was given")

// TargetExistsError is returned by PlanRestore when the target database
// already exists. dbsnap never restores into an existing database, even
// an empty one — Suggestion is a free name the caller can offer instead.
type TargetExistsError struct {
	DBName     string
	Suggestion string
}

func (e *TargetExistsError) Error() string {
	return fmt.Sprintf("database %q already exists", e.DBName)
}

// RestorePlan is the outcome of PlanRestore: which database a restore will
// create, and what's known about the backup it'll be restored from.
type RestorePlan struct {
	// DBName is the database that will be created and restored into.
	DBName string
	// NameFromArchive is true when DBName wasn't requested explicitly and
	// was taken from the name recorded inside the backup instead.
	NameFromArchive bool
	Archive         ArchiveInfo
}

// PlanRestore is the pre-flight check for a restore, shared by the
// interactive and non-interactive flows: it reads the backup's own
// metadata, resolves the target database name (conn.DBName if set,
// otherwise the name recorded in the backup), and refuses if that
// database already exists on the server.
//
// The archive and existence checks only run if p implements
// ArchiveInspector / Inspector respectively. Without Inspector the
// existence check falls to Provider.Restore itself, which must refuse an
// existing database anyway.
func PlanRestore(ctx context.Context, p Provider, conn ConnectionInfo, opts RestoreOptions) (RestorePlan, error) {
	var plan RestorePlan

	info, err := os.Stat(opts.InputPath)
	if err != nil {
		return plan, fmt.Errorf("cannot read backup at %q: %w", opts.InputPath, err)
	}
	if !info.IsDir() && info.Size() == 0 {
		return plan, fmt.Errorf("backup at %q is empty", opts.InputPath)
	}

	if ai, ok := p.(ArchiveInspector); ok {
		plan.Archive, err = ai.InspectArchive(ctx, opts.InputPath)
		if err != nil {
			return plan, err
		}
	}

	plan.DBName, err = ResolveRestoreName(conn.DBName, plan.Archive)
	if err != nil {
		return plan, err
	}
	plan.NameFromArchive = conn.DBName == ""

	if insp, ok := p.(Inspector); ok {
		existing, err := insp.ListDatabases(ctx, conn)
		if err != nil {
			return plan, err
		}
		for _, n := range existing {
			if n == plan.DBName {
				return plan, &TargetExistsError{
					DBName:     plan.DBName,
					Suggestion: SuggestRestoreName(plan.DBName, existing, time.Now()),
				}
			}
		}
	}
	return plan, nil
}

// ResolveRestoreName picks the database a restore should create: the
// explicitly requested name if there is one, otherwise the name recorded
// inside the backup. It returns ErrNoDBName if neither is available.
func ResolveRestoreName(requested string, archive ArchiveInfo) (string, error) {
	if requested != "" {
		return requested, nil
	}
	if archive.DBName != "" {
		return archive.DBName, nil
	}
	return "", ErrNoDBName
}

// SuggestRestoreName proposes a database name derived from base that
// doesn't collide with any name in existing: base_restore_YYYYMMDD, then
// base_restore_YYYYMMDD_2, _3, ... if that's taken too.
func SuggestRestoreName(base string, existing []string, now time.Time) string {
	taken := make(map[string]bool, len(existing))
	for _, n := range existing {
		taken[n] = true
	}
	candidate := fmt.Sprintf("%s_restore_%s", base, now.Format("20060102"))
	for i := 2; taken[candidate]; i++ {
		candidate = fmt.Sprintf("%s_restore_%s_%d", base, now.Format("20060102"), i)
	}
	return candidate
}
