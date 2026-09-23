package core

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveRestoreName(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		archive   ArchiveInfo
		want      string
		wantErr   error
	}{
		{"explicit name wins over archive", "appdb_restore", ArchiveInfo{DBName: "appdb"}, "appdb_restore", nil},
		{"explicit name without archive name", "appdb", ArchiveInfo{}, "appdb", nil},
		{"falls back to archive name", "", ArchiveInfo{DBName: "appdb"}, "appdb", nil},
		{"no name anywhere", "", ArchiveInfo{Format: "plain"}, "", ErrNoDBName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveRestoreName(tt.requested, tt.archive)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSuggestRestoreName(t *testing.T) {
	now := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		existing []string
		want     string
	}{
		{"free on first try", []string{"appdb"}, "appdb_restore_20260922"},
		{"skips taken suggestion", []string{"appdb", "appdb_restore_20260922"}, "appdb_restore_20260922_2"},
		{"skips several", []string{"appdb_restore_20260922", "appdb_restore_20260922_2"}, "appdb_restore_20260922_3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SuggestRestoreName("appdb", tt.existing, now); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// fakeProvider implements Provider, Inspector and ArchiveInspector with
// canned answers, recording whether Restore was ever called.
type fakeProvider struct {
	archive  ArchiveInfo
	existing []string
	restored bool
}

func (f *fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Backup(context.Context, ConnectionInfo, BackupOptions) (BackupResult, error) {
	return BackupResult{}, nil
}
func (f *fakeProvider) Restore(context.Context, ConnectionInfo, RestoreOptions) (RestoreResult, error) {
	f.restored = true
	return RestoreResult{}, nil
}
func (f *fakeProvider) Ping(context.Context, ConnectionInfo) error { return nil }
func (f *fakeProvider) ListDatabases(context.Context, ConnectionInfo) ([]string, error) {
	return f.existing, nil
}
func (f *fakeProvider) Inspect(context.Context, ConnectionInfo) (DatabaseInspection, error) {
	return DatabaseInspection{}, nil
}
func (f *fakeProvider) InspectArchive(context.Context, string) (ArchiveInfo, error) {
	return f.archive, nil
}

func writeBackup(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "appdb_20260721T101500.dump")
	if err := os.WriteFile(path, []byte("not empty"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPlanRestore(t *testing.T) {
	path := writeBackup(t)

	t.Run("name from archive, target free", func(t *testing.T) {
		p := &fakeProvider{archive: ArchiveInfo{DBName: "appdb"}, existing: []string{"postgres"}}
		plan, err := PlanRestore(context.Background(), p, ConnectionInfo{}, RestoreOptions{InputPath: path})
		if err != nil {
			t.Fatal(err)
		}
		if plan.DBName != "appdb" || !plan.NameFromArchive {
			t.Errorf("plan = %+v, want DBName appdb taken from archive", plan)
		}
	})

	t.Run("explicit name, target free", func(t *testing.T) {
		p := &fakeProvider{archive: ArchiveInfo{DBName: "appdb"}, existing: []string{"appdb"}}
		plan, err := PlanRestore(context.Background(), p, ConnectionInfo{DBName: "appdb_restore"}, RestoreOptions{InputPath: path})
		if err != nil {
			t.Fatal(err)
		}
		if plan.DBName != "appdb_restore" || plan.NameFromArchive {
			t.Errorf("plan = %+v, want explicit DBName appdb_restore", plan)
		}
	})

	t.Run("refuses existing database, even from archive name", func(t *testing.T) {
		p := &fakeProvider{archive: ArchiveInfo{DBName: "appdb"}, existing: []string{"appdb"}}
		_, err := PlanRestore(context.Background(), p, ConnectionInfo{}, RestoreOptions{InputPath: path})
		var exists *TargetExistsError
		if !errors.As(err, &exists) {
			t.Fatalf("err = %v, want *TargetExistsError", err)
		}
		if exists.DBName != "appdb" || exists.Suggestion == "" || exists.Suggestion == "appdb" {
			t.Errorf("got %+v, want appdb with a different suggestion", exists)
		}
		if p.restored {
			t.Error("Restore was called despite the target existing")
		}
	})

	t.Run("refuses existing explicit name", func(t *testing.T) {
		p := &fakeProvider{archive: ArchiveInfo{DBName: "appdb"}, existing: []string{"appdb_restore"}}
		_, err := PlanRestore(context.Background(), p, ConnectionInfo{DBName: "appdb_restore"}, RestoreOptions{InputPath: path})
		var exists *TargetExistsError
		if !errors.As(err, &exists) || exists.DBName != "appdb_restore" {
			t.Fatalf("err = %v, want *TargetExistsError for appdb_restore", err)
		}
	})

	t.Run("no name recorded and none given", func(t *testing.T) {
		p := &fakeProvider{archive: ArchiveInfo{Format: "plain"}}
		_, err := PlanRestore(context.Background(), p, ConnectionInfo{}, RestoreOptions{InputPath: path})
		if !errors.Is(err, ErrNoDBName) {
			t.Fatalf("err = %v, want ErrNoDBName", err)
		}
	})

	t.Run("missing backup file", func(t *testing.T) {
		p := &fakeProvider{archive: ArchiveInfo{DBName: "appdb"}}
		_, err := PlanRestore(context.Background(), p, ConnectionInfo{}, RestoreOptions{InputPath: filepath.Join(t.TempDir(), "nope.dump")})
		if err == nil {
			t.Fatal("expected an error for a missing backup file")
		}
	})
}
