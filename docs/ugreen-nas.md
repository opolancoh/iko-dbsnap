# Running on a UGREEN NAS (Docker)

UGOS has no standard package manager, so the native install steps in the
main [README](../README.md) don't apply. Instead, use the included
[`Dockerfile`](../Dockerfile), which bundles the Go build with
`postgresql-client`, and run it via UGOS's **Docker** app.

DXP2800 is x86_64 — no cross-compilation needed.

## Prerequisites

- Admin access to the NAS's UGOS web UI.
- NAS on the same network as the Postgres server (or otherwise routable).
- SSH access to the NAS (optional — Docker app's UI can do
  everything below without it).

## First-time setup

1. **Enable SSH**: *Control Panel → Terminal* (under *Connection &
   Access*) → turn on SSH. Make sure the change actually saves/applies
   (re-check the page) — a toggle that looks on but wasn't applied
   yet still refuses connections on port 22.
2. **Install Docker app**: *App Center* → search *Docker* → install.
3. **Set up passwordless SSH** — `scripts/deploy-nas.sh` connects with
   `BatchMode=yes`, which fails outright instead of prompting for a
   password:

   ```sh
   ssh <admin-user>@<nas-ip>          # accept the host key fingerprint once
   ssh-copy-id <admin-user>@<nas-ip>  # installs your public key
   ```

4. **Add the admin user to the `docker` group** — otherwise every
   `docker` command over SSH fails with "permission denied while trying
   to connect to the docker API":

   ```sh
   ssh -t <admin-user>@<nas-ip> "sudo usermod -aG docker <admin-user>"
   ```

   Group membership only takes effect on a new SSH session, so this
   needs its own `-t` (interactive, for the sudo password) rather than
   running through the deploy script.

5. **Verify**: `ssh <admin-user>@<nas-ip> docker version` should print
   both client *and* server info without any prompts.

## 1. Ship the image to the NAS

From your dev machine, with SSH access to the NAS:

```sh
./scripts/deploy-nas.sh --host <user@nas-ip>
```

This builds the image locally (targeting `linux/amd64`, matching the
DXP2800) and loads it into the NAS's Docker over SSH — no registry, no
copying the repo itself onto the NAS.

Alternatively, without the script: copy the repo onto the NAS (`scp`,
File Station, or `git clone`) and either run `docker build -t dbsnap .`
over SSH, or use the Docker app's UI ("Build", pointing at the folder
with the `Dockerfile`).

### pg_dump version pin

The `Dockerfile` installs `postgresql-client-18` from the PostgreSQL
[PGDG apt repo](https://wiki.postgresql.org/wiki/Apt), **not** Debian
bookworm's base `postgresql-client` (which is version 15). This is
deliberate: `pg_dump` must be at least as new as the server it dumps, or
the backup fails with `server version: X; pg_dump version: Y` and writes
zero rows. Version 18 is backward compatible and covers servers 15–18.

**If you add a PostgreSQL 19+ server**, bump `postgresql-client-18` to the
matching version in the `Dockerfile` and re-ship the image. Nothing in the
Go code needs to change — dbsnap calls `pg_dump` from `PATH`, and the PGDG
wrapper resolves it to whatever version is installed.

## 2. Run it

```sh
# -host must be the Postgres server's LAN IP/hostname, not "localhost"
# (unless Postgres also runs on the NAS)
sudo docker run --rm -it \
  -e DBSNAP_PASSWORD=secret \
  -v /volume1/docker/dbsnap/backups:/data/backups \
  dbsnap backup -host 192.168.1.50 -user postgres -db appdb -out /data/backups
```

Equivalent Docker app UI settings:

- volume mapping: `/volume1/docker/dbsnap/backups` → `/data/backups`
- environment variable: `DBSNAP_PASSWORD`
- command: `backup -host 192.168.1.50 -user postgres -db appdb -out /data/backups`

If Postgres runs in another container on the same NAS, put both
containers on the same Docker network and use that container's name as
`-host`.

## 3. Scheduling backups

UGOS has no Task Scheduler UI (unlike Synology DSM), so schedule with
**cron** over SSH. Run a wrapper script rather than a raw `docker run` —
it gives you logging and a real exit code.

**Keep the password out of the command line** (it's otherwise visible in
`ps` and `docker inspect`):

```sh
# write the password to a file instead of the command line
echo 'DBSNAP_PASSWORD=secret' | sudo tee /volume1/docker/dbsnap/dbsnap.env >/dev/null
# make it readable only by root
sudo chmod 600 /volume1/docker/dbsnap/dbsnap.env
```

Find docker's absolute path (Task Scheduler runs with a minimal `PATH`):

```sh
command -v docker            # commonly /usr/bin/docker
```

Create the wrapper script. The database name is passed as an argument, so
one script can back up any DB:

```sh
# writes everything between the EOF markers into the script file
sudo tee /volume1/docker/dbsnap/run-backup.sh >/dev/null <<'EOF'
#!/bin/sh
DB="${1:?usage: run-backup.sh <db-name>}"   # DB name from the first argument
DOCKER=/usr/bin/docker                      # set to your `command -v docker`
LOG=/volume1/docker/dbsnap/backup.log       # run output is appended here
ts() { date '+%Y-%m-%d %H:%M:%S'; }         # timestamp helper for the log

echo "===== $(ts) starting $DB =====" >> "$LOG"
"$DOCKER" run --rm \
  --env-file /volume1/docker/dbsnap/dbsnap.env \
  -v /volume1/docker/dbsnap/backups:/data/backups \
  dbsnap backup -host 192.168.1.50 -user postgres -db "$DB" \
    -out /data/backups -non-interactive >> "$LOG" 2>&1
status=$?                                    # remember the container's exit code

# after a successful backup, delete dumps older than 50 days (skipped on a
# failed run, so a broken backup can't wipe your last good copies)
if [ "$status" -eq 0 ]; then
  find /volume1/docker/dbsnap/backups -name '*.dump' -mtime +50 -delete
fi

echo "===== $(ts) finished $DB, exit $status =====" >> "$LOG"
exit $status                                 # hand the exit code to cron
EOF
```

Make it executable:

```sh
sudo chmod +x /volume1/docker/dbsnap/run-backup.sh
```

Before running it, edit two placeholders in the script:

- `-host` — your Postgres server's IP or hostname.
- `DOCKER=` — the path from `command -v docker` above.

Two things the script does on purpose: it always writes the `finished`
line to the log even when the backup fails (the log never cuts off
mid-run), and it exits with the same status the backup returned — so cron,
or you, can tell success from failure.

**Retention:** after a successful run it deletes `.dump` files older than
50 days. It's guarded by `[ "$status" -eq 0 ]`, so a failing backup never
prunes anything — you can't end up with zero copies. Change `+50` to keep
a different number of days.

Now test it once. `echo $?` prints that exit status: `0` means success;
any other number means it failed, so check `backup.log`.

```sh
sudo /volume1/docker/dbsnap/run-backup.sh db_name; echo $?
```

Confirm the dump landed and the run logged cleanly:

```sh
sudo ls -la /volume1/docker/dbsnap/backups      # a fresh, non-zero .dump
sudo tail -3 /volume1/docker/dbsnap/backup.log  # ...finished db_name, exit 0
```

Schedule it in root's crontab (root, so it can read the 0600 env file and
reach the Docker socket):

```sh
sudo crontab -e
```

Add a daily 02:45 job — the DB name is the argument:

```cron
45 2 * * * /volume1/docker/dbsnap/run-backup.sh db_name
```

Verify with `sudo crontab -l`. Cron uses the NAS's local clock, so set the
timezone to America/Bogotá (COT) under *Control Panel → Time & Language*;
02:45 then means 02:45 COT. The morning after, confirm a fresh dump landed
and check `backup.log` for the outcome.
