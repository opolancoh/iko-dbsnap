package postgres

import (
	"reflect"
	"testing"
)

func TestParseIgnoredErrors(t *testing.T) {
	stderr := `pg_restore: connecting to database for restore
pg_restore: error: could not execute query: ERROR:  unrecognized configuration parameter "transaction_timeout"
Command was: SET transaction_timeout = 0;
pg_restore: creating SCHEMA "app"
pg_restore: warning: errors ignored on restore: 1
`
	got, ok := parseIgnoredErrors(stderr)
	if !ok {
		t.Fatal("expected the ignored-errors summary to be recognized")
	}
	want := []string{`could not execute query: ERROR:  unrecognized configuration parameter "transaction_timeout" (Command was: SET transaction_timeout = 0;)`}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}

	// No summary line means pg_restore stopped early: a real failure.
	if _, ok := parseIgnoredErrors("pg_restore: error: connection to server failed\n"); ok {
		t.Error("a failure without the ignored-errors summary must not be treated as a warning")
	}
}
