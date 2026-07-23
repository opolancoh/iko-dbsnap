# dbsnap

A CLI to back up and restore databases through a pluggable provider
architecture. PostgreSQL is supported via the standard `pg_dump` /
`pg_restore` / `psql` client tools.

## Requirements

- Go 1.25+ (only needed to build the binary).
- Network access to the Postgres server you're backing up/restoring, and
  a user with enough privileges to read the databases involved (and, for
  discovery, to list databases and read `pg_catalog`).
- The PostgreSQL client tools on `PATH`: `pg_dump`, `pg_restore`, `psql`.
  dbsnap shells out to these rather than reimplementing dump/restore
  logic itself.

  **Version compatibility:** `pg_dump` must be **at least as new as the
  server** it backs up — it refuses to dump a newer server and fails with
  `server version: X; pg_dump version: Y`. It *is* backward compatible, so
  a newer client dumps older servers fine. When in doubt, install the
  newest `pg_dump` you'll need across all your servers. (The Docker image
  pins version 18 for exactly this reason — see
  [docs/ugreen-nas.md](docs/ugreen-nas.md).)

  Check whether you already have them:

  ```sh
  # prints version if installed, "command not found" if not
  pg_dump --version && pg_restore --version && psql --version
  ```

  If that fails, install them:

  **macOS (Homebrew)**

  ```sh
  # installs the client tools (libpq includes pg_dump/pg_restore/psql)
  brew install libpq
  # libpq is keg-only by default; this puts pg_dump/pg_restore/psql on PATH
  brew link --force libpq
  ```

  **Linux (Debian/Ubuntu)**

  ```sh
  # refreshes package lists, then installs the client tools
  sudo apt-get update && sudo apt-get install -y postgresql-client
  ```

  **Linux (Fedora/RHEL/CentOS)**

  ```sh
  # installs the client tools (pulls in pg_dump/pg_restore/psql)
  sudo dnf install -y postgresql
  ```

  **Linux (Arch)**

  ```sh
  # installs the client tools (pulls in pg_dump/pg_restore/psql)
  sudo pacman -S postgresql-libs
  ```

  **UGREEN NAS (UGOS)**

  UGOS doesn't have a standard package manager — see
  [docs/ugreen-nas.md](docs/ugreen-nas.md) for a Docker-based setup.

## Build

```sh
# compiles the CLI into ./bin/dbsnap
go build -o bin/dbsnap ./cmd/dbsnap
```

This produces a `dbsnap` binary under `bin/`. Everything below assumes
you run it as `./bin/dbsnap` (or put it on your `PATH`).

## Quick start: backing up

dbsnap needs the database password before it can connect — set it via
`DBSNAP_PASSWORD` (preferred, keeps it out of shell history/`ps`) or pass
`-password` directly:

```sh
# sets the password for this shell session
export DBSNAP_PASSWORD=secret

# backs up database "appdb" on localhost, connecting as user "postgres";
# picks up DBSNAP_PASSWORD automatically, no -password flag needed
./bin/dbsnap backup -host localhost -user postgres -db appdb
```

Backups are written to `./backups` by default — pass `-out <dir>` to
change it.

You'll be prompted for nothing further on the command line — instead, on
a terminal, dbsnap walks through an interactive flow:

1. **Connect** — verifies the host/credentials are reachable.
2. **Discover** — checks which of the databases you asked for actually
   exist, and reports any that don't.
3. **Inspect** — for each database found, lists every schema and table
   with an exact row count (`SELECT count(*)` per table — accurate, but
   it's a full scan, so this step can take a while on large tables).
4. **Confirm** — shows the full report and asks `Continue with backup?
   [y/N]` before touching anything.
5. **Backup** — runs `pg_dump` per database, showing which table is
   currently being dumped as it goes.
6. **Summary** — a final schema/table breakdown of rows found vs. rows
   backed up, plus totals.

### Backing up several databases at once

```sh
# backs up 3 databases, at most 3 pg_dump processes running at once
./bin/dbsnap backup -user postgres -db appdb,billingdb,analyticsdb -concurrency 3
```

Each named database gets backed up independently (own `pg_dump` process),
up to `-concurrency` running in parallel. If some of the names don't
exist on the server, dbsnap still proceeds with the ones that do — it
only refuses to continue if *none* of them are found.

### Skipping the confirmation prompt

For scripted-but-still-interactive runs (you still want to see the
report and progress, just not be asked to press `y`):

```sh
# runs the full report + progress UI, but proceeds without asking y/n
./bin/dbsnap backup -user postgres -db appdb -yes
```

### Non-interactive / cron use

When stdout isn't a terminal (e.g. running under cron, or piped to a
file), dbsnap automatically skips the interactive flow and falls back to
plain line-per-database output — no flags needed. You can also force this
on a real terminal with `-non-interactive`:

```sh
# forces plain OK/FAIL output instead of the interactive flow
./bin/dbsnap backup -user postgres -db appdb,billingdb -non-interactive
```

```
# one line per database: OK with size/duration/path, or FAIL with the error
OK   appdb                             1048576 bytes      812ms  -> ./backups/appdb_20260721T101500.dump
FAIL billingdb                         connection refused
```

Exit code is non-zero if any database failed to back up.

### Backup flags

| Flag           | Default      | Meaning                                                                 |
|----------------|--------------|--------------------------------------------------------------------------|
| `-provider`    | `postgres`   | Which engine to use.                                                    |
| `-host`        | `localhost`  | Database host.                                                          |
| `-port`        | provider default (5432 for postgres) | Database port.                                  |
| `-user`        | *(required)* | Database user.                                                          |
| `-password`    | `$DBSNAP_PASSWORD` | Database password.                                                 |
| `-db`          | *(required)* | Comma-separated database names to back up.                              |
| `-out`         | `./backups`  | Directory to write backup files to.                                     |
| `-format`      | provider default (`custom` for postgres) | Backup format — postgres supports `custom`, `plain`, `directory`, `tar`. |
| `-concurrency` | `3`          | How many databases to back up in parallel.                              |
| `-yes`         | `false`      | Skip the confirmation prompt.                                           |
| `-non-interactive` | `false`  | Force plain output even on a terminal.                                  |

## Restoring

Restore takes one or more `dbname=path` pairs — each target database
needs its own source file, since (unlike backup) there's no way to infer
where each database's data should come from.

```sh
# restores that one file into database "appdb" on localhost, as user "postgres"
./bin/dbsnap restore -host localhost -user postgres \
  -db "appdb=./backups/appdb_20260721T101500.dump"
```

Several at once:

```sh
# restores two databases in parallel, each from its own backup file
./bin/dbsnap restore -host localhost -user postgres \
  -db "appdb=./backups/appdb_20260721T101500.dump,billingdb=./backups/billingdb_20260721T101500.dump" \
  -concurrency 2
```

If a target database already exists, dbsnap restores into it as-is —
conflicting objects will error. There's deliberately no option to drop
existing objects first; if you want a clean slate, drop the database
yourself first (a conscious, hard-to-do-by-accident action), then
restore — since it no longer exists, dbsnap creates it itself under
exactly the name you asked for (using server default encoding/owner/
collation) before restoring into it. That target name doesn't have to
match the database the backup originally came from — restoring
`appdb=./backups/appdb_....dump` as `-db "appdb_staging=..."` creates
`appdb_staging`, not `appdb`. If the backup's original name is on record
(archive formats only) and differs from your target, the report just
notes it for context; it never blocks the restore.

### Restore flags

| Flag           | Default      | Meaning                                                                 |
|----------------|--------------|--------------------------------------------------------------------------|
| `-provider`    | `postgres`   | Which engine to use.                                                    |
| `-host`        | `localhost`  | Database host.                                                          |
| `-port`        | provider default | Database port.                                                      |
| `-user`        | *(required)* | Database user.                                                          |
| `-password`    | `$DBSNAP_PASSWORD` | Database password.                                                 |
| `-db`          | *(required)* | Comma-separated `dbname=path` pairs to restore.                         |
| `-format`      | inferred per file | Force a format instead of inferring it from the file/directory.    |
| `-no-owner`    | `false`      | Skip restoring ownership/ACLs — useful when the target role differs from the one that produced the backup. |
| `-jobs`        | —            | Parallel restore jobs (postgres: only for `custom`/`directory` format dumps). |
| `-concurrency` | `3`          | How many databases to restore in parallel.                              |

## Notes and caveats

- **Row counts are exact, not free.** `-db` discovery runs `SELECT
  count(*)` per table (a few at a time, bounded concurrency) to get exact
  numbers. On databases with very large tables this adds real time before
  the backup itself starts.
- **The "rows backed up" figure is trusted from `pg_dump`'s exit code**,
  not independently re-counted — `pg_dump` doesn't report row-level
  progress, so a successful exit is taken to mean everything listed in
  the pre-flight report was captured.
- **Discovery requires connecting to a maintenance database** (default
  `postgres`) to list what else exists on the server, since Postgres has
  no "connect with no database" mode. Override it via a connection param
  if your server doesn't have one (see `providers/postgres/inspect.go`).
