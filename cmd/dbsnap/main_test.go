package main

import (
	"strings"
	"testing"

	"iko-dbsnap/core"
)

func TestParseRestoreArgs(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantDB   string
		wantFile string
		wantErr  string
	}{
		{"file only", []string{"-user", "postgres", "./backups/appdb.dump"}, "", "./backups/appdb.dump", ""},
		{"db before file", []string{"-user", "postgres", "-db", "appdb_restore", "./backups/appdb.dump"}, "appdb_restore", "./backups/appdb.dump", ""},
		{"flags after file", []string{"-user", "postgres", "./backups/appdb.dump", "-db", "appdb_restore"}, "appdb_restore", "./backups/appdb.dump", ""},
		{"missing file", []string{"-user", "postgres", "-db", "appdb"}, "", "", "missing the backup file"},
		{"two files", []string{"-user", "postgres", "a.dump", "b.dump"}, "", "", "exactly one backup file"},
		{"missing user", []string{"./backups/appdb.dump"}, "", "", "-user"},
		{"old name=path syntax", []string{"-user", "postgres", "-db", "appdb=./backups/appdb.dump"}, "", "", "pass the backup file as an argument"},
		{"several databases", []string{"-user", "postgres", "-db", "a,b", "a.dump"}, "", "", "single database name"},
		{"removed -concurrency flag", []string{"-user", "postgres", "-concurrency", "2", "a.dump"}, "", "", "-concurrency"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := parseRestoreArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.db != tt.wantDB || a.file != tt.wantFile {
				t.Errorf("db=%q file=%q, want db=%q file=%q", a.db, a.file, tt.wantDB, tt.wantFile)
			}
		})
	}
}

func TestRestoreCommand(t *testing.T) {
	a := restoreArgs{provider: "postgres", host: "db.internal", port: 5433, user: "postgres", noOwner: true, file: "/tmp/my backups/appdb.dump"}
	got := restoreCommand("dbsnap", a, "appdb_restore_20260922")
	want := "dbsnap restore -host db.internal -port 5433 -user postgres -no-owner -db appdb_restore_20260922 '/tmp/my backups/appdb.dump'"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}

	a = restoreArgs{provider: "postgres", host: "localhost", user: "postgres", file: "./backups/appdb.dump"}
	got = restoreCommand("./bin/dbsnap", a, "appdb")
	want = "./bin/dbsnap restore -user postgres -db appdb ./backups/appdb.dump"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestRestoreErrorMessage(t *testing.T) {
	a := restoreArgs{provider: "postgres", host: "localhost", user: "postgres", file: "./backups/appdb.dump"}
	conn := core.ConnectionInfo{Host: "localhost"}

	msg := restoreErrorMessage("dbsnap", a, conn, &core.TargetExistsError{DBName: "appdb", Suggestion: "appdb_restore_20260922"})
	for _, want := range []string{`"appdb"`, "already exists", "nothing was restored", "name taken from the backup", "-db appdb_restore_20260922 ./backups/appdb.dump"} {
		if !strings.Contains(msg, want) {
			t.Errorf("exists message missing %q:\n%s", want, msg)
		}
	}

	msg = restoreErrorMessage("dbsnap", a, conn, core.ErrNoDBName)
	for _, want := range []string{"does not record a database name", "-db mydb ./backups/appdb.dump"} {
		if !strings.Contains(msg, want) {
			t.Errorf("no-name message missing %q:\n%s", want, msg)
		}
	}
}
