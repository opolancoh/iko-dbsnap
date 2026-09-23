# dbsnap

A command-line tool to back up and restore databases. It currently
supports PostgreSQL, using the standard `pg_dump`, `pg_restore` and `psql`
tools.

## Requirements

- **Go 1.25+**, only to build the binary.
- **PostgreSQL client tools** (`pg_dump`, `pg_restore`, `psql`) on your
  `PATH`.
- **A database user** that can read the databases you back up. To
  restore, it also needs permission to create databases.

**Version rule:** `pg_dump` must be **the same version as the server or
newer**. An older `pg_dump` refuses to back up a newer server
(`server version: X; pg_dump version: Y`). When in doubt, install the
newest version.

Check what you have:

```sh
pg_dump --version && pg_restore --version && psql --version
```

Install them if needed:

| System | Command |
|---|---|
| macOS (Homebrew) | `brew install libpq && brew link --force libpq` |
| Debian / Ubuntu | `sudo apt-get update && sudo apt-get install -y postgresql-client` |
| Fedora / RHEL / CentOS | `sudo dnf install -y postgresql` |
| Arch | `sudo pacman -S postgresql-libs` |
| UGREEN NAS (UGOS) | No package manager, so use Docker. See [docs/ugreen-nas.md](docs/ugreen-nas.md). |

## Build

```sh
go build -o bin/dbsnap ./cmd/dbsnap
```

The examples below run it as `./bin/dbsnap`.

## Connecting

Both commands take the same connection flags: `-host` (default
`localhost`), `-port` (default `5432`), `-user` (required) and the
password.

Set the password as an environment variable, so it stays out of your
shell history:

```sh
export DBSNAP_PASSWORD='your-password'
```

You can also pass `-password`, but the password then shows up in your
shell history.

## Backing up

```sh
./bin/dbsnap backup -user postgres -db appdb
```

This writes `./backups/appdb_<date>T<time>.dump`. Use `-out <dir>` to
choose another folder.

On a terminal, dbsnap first shows what it found (every schema and table,
with row counts) and asks `Continue with backup? [y/N]`. After the backup,
it prints a summary.

**Several databases at once:**

```sh
./bin/dbsnap backup -user postgres -db appdb,billingdb,analyticsdb
```

Up to 3 run in parallel (change with `-concurrency`). On a terminal, names
that don't exist are reported and skipped, and dbsnap stops only if none
of them exist. In plain output, each missing name shows up as a `FAIL`
line.

**Scripts and cron:** when the output isn't a terminal, or with
`-non-interactive`, dbsnap skips the questions and prints one line per
database:

```
OK   appdb       1048576 bytes   812ms  -> ./backups/appdb_20260721T101500.dump
FAIL billingdb   connection refused
```

The exit code is non-zero if any backup failed. To keep the interactive
screens but skip the question, use `-yes`.

### Backup flags

| Flag | Default | Meaning |
|---|---|---|
| `-db` | *(required)* | Databases to back up, comma-separated. |
| `-out` | `./backups` | Folder for the backup files. |
| `-format` | `custom` | `custom` (compressed, recommended), `plain` (`.sql` text), `directory` or `tar`. |
| `-concurrency` | `3` | How many databases to back up at the same time. |
| `-yes` | off | Don't ask for confirmation. |
| `-non-interactive` | off | Plain one-line output, even on a terminal. |
| `-host`, `-port`, `-user`, `-password`, `-provider` | | See [Connecting](#connecting). `-provider` is `postgres`, the only one available. |

## Restoring

**A restore always creates a new database.** dbsnap never writes into a
database that already exists, even an empty one, so an existing database
can't be overwritten by accident.

```sh
# creates "appdb" (the name stored in the backup)
./bin/dbsnap restore -user postgres ./backups/appdb_20260721T101500.dump

# creates "appdb_restore" instead
./bin/dbsnap restore -user postgres -db appdb_restore ./backups/appdb_20260721T101500.dump
```

Pass one backup file per command. Flags can go before or after the file.

### How the new database is named

1. The name you pass with **`-db`**.
2. Otherwise, **the name stored inside the backup**. `custom`, `tar` and
   `directory` backups store it.
3. Otherwise, dbsnap stops and asks for `-db`. `.sql` backups don't store
   a name.

dbsnap never takes the name from the file name.

### If the name is already taken

dbsnap stops before changing anything and suggests a free name, as a
command you can copy:

```
Database "appdb" (name taken from the backup) already exists on localhost — nothing was restored.
dbsnap only restores into a new database it creates itself. Choose another name with -db, e.g.:
  ./bin/dbsnap restore -user postgres -db appdb_restore_20260922 ./backups/appdb_20260721T101500.dump
```

### Replacing a database safely

Restore under a new name, check the data, then swap the names:

```sh
./bin/dbsnap restore -user postgres -db appdb_restore ./backups/appdb_20260721T101500.dump
```

```sql
-- run while connected to another database, e.g. "postgres"
ALTER DATABASE appdb RENAME TO appdb_old;
ALTER DATABASE appdb_restore RENAME TO appdb;
```

Postgres refuses to rename a database while anyone is connected to it, so
stop your app first. Keep `appdb_old` until you're sure, then delete it
with `DROP DATABASE appdb_old;`.

For a **test copy**, skip the rename: restore under a new name, use it,
and `DROP DATABASE` it when you're done.

### What you'll see

On a terminal, dbsnap shows the server, the database it will create and
the backup file, then asks `Continue with restore? [y/N]`. After the
restore, it counts the rows in the new database so you can check the
result. `-yes` skips the question. `-non-interactive` (or no terminal)
prints a single `OK` or `FAIL` line.

- **Warnings:** sometimes a single statement fails and is skipped while
  the rest restores fine. Usually this is a setting the server doesn't
  support (see [Notes](#notes)). dbsnap still reports success but lists
  every skipped statement, marked ⚠ (or `WARN` in plain output). Read
  them: occasionally they mean something wasn't restored.
- **Failures:** if the restore fails partway, the new database stays
  behind and may be incomplete. Drop it before trying again.
- **"role does not exist" errors:** the backup records which user owns
  each table. If that user doesn't exist on this server, add `-no-owner`
  and your user will own everything instead.

### Restore flags

| Flag | Default | Meaning |
|---|---|---|
| `-db` | name stored in the backup | Name of the new database. It must not exist yet. |
| `-no-owner` | off | Don't restore the original owners and permissions. Everything belongs to `-user`. |
| `-format` | detected from the file | Set the backup format when detection gets it wrong. |
| `-jobs` | | Restore with several parallel workers (faster for large `custom` or `directory` backups). |
| `-yes` | off | Don't ask for confirmation. |
| `-non-interactive` | off | Plain one-line output, even on a terminal. |
| `-host`, `-port`, `-user`, `-password`, `-provider` | | See [Connecting](#connecting). |

## Notes

- **Restoring into an older Postgres version.** A backup made by a newer
  `pg_dump` can contain settings an older server doesn't recognize. For
  example, `pg_dump` 17+ writes `SET transaction_timeout = 0;`, which
  Postgres 16 rejects. For `custom`, `tar` and `directory` backups, that
  statement is skipped and shown as a warning. A `.sql` backup stops at
  the first error, so the restore fails. Restoring into a server of the
  same version or newer avoids this.
- **Row counts take time.** Before a backup, and after a restore, dbsnap
  counts every table's rows exactly (`SELECT count(*)`). On very large
  tables this can take a while.
- **The backup summary trusts `pg_dump`.** Row counts are taken before
  the backup. If `pg_dump` finishes without errors, those rows are
  assumed to be in the file. They aren't re-counted from the file.
- **A `postgres` database must exist on the server.** dbsnap connects to
  it to list databases and to create new ones. This can't be changed from
  the command line yet.
