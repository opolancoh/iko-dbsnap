# Running on a UGREEN NAS (Docker)

UGOS has no package manager, so the README's native install steps don't
apply. Instead, build the included [`Dockerfile`](../Dockerfile) and run it
via UGOS's **Docker** app. The DXP2800 is x86_64, so no cross-compilation.

## Prerequisites

- Admin access to the UGOS web UI.
- The NAS able to reach the Postgres server over the network.
- SSH access (optional — the Docker app UI can do everything below).

## First-time setup

1. **Enable SSH** — *Control Panel → Terminal* → turn on SSH. Re-check the
   page to confirm it applied; a toggle that looks on but didn't save still
   refuses port 22.
2. **Install Docker** — *App Center* → search *Docker* → install.
3. **Passwordless SSH** — `deploy-nas.sh` uses `BatchMode=yes`, which won't
   prompt for a password:

   ```sh
   ssh <user>@<nas-ip>          # accept the host key once
   ssh-copy-id <user>@<nas-ip>  # install your public key
   ```

4. **Add your user to the `docker` group** — otherwise `docker` over SSH
   fails with permission denied on the Docker socket:

   ```sh
   ssh -t <user>@<nas-ip> "sudo usermod -aG docker <user>"  # -t: prompts for sudo
   ```

   Takes effect on the next SSH session.

5. **Verify** — `ssh <user>@<nas-ip> docker version` prints client and
   server info with no prompts.

## 1. Ship the image to the NAS

From your dev machine:

```sh
./scripts/deploy-nas.sh --host <user@nas-ip>
```

It builds for `linux/amd64` and loads the image into the NAS's Docker over
SSH — no registry, no copying the repo. (The image pins the Postgres client
to v18; see [pg_dump version](#pg_dump-version).)

Without the script: copy the repo to the NAS and run `docker build -t
dbsnap .`, or use the Docker app's **Build** UI.

## 2. Run it

```sh
# -host is the Postgres server's LAN IP/hostname, not localhost
# (unless Postgres also runs on the NAS)
sudo docker run --rm -it \
  -e DBSNAP_PASSWORD=secret \
  -v /volume1/docker/dbsnap/backups:/data/backups \
  dbsnap backup -host 192.168.1.50 -user postgres -db appdb -out /data/backups
```

Equivalent Docker app UI settings:

- volume: `/volume1/docker/dbsnap/backups` → `/data/backups`
- environment: `DBSNAP_PASSWORD`
- command: `backup -host 192.168.1.50 -user postgres -db appdb -out /data/backups`

If Postgres runs in another container on the same NAS, put both on one
Docker network and use that container's name as `-host`.

## 3. Schedule backups

UGOS has no Task Scheduler UI, so use **cron**. Run a
wrapper script rather than a raw `docker run` — you get logging and a real
exit code.

Store the password in a file, not the command line (a `-e` value is visible
in `ps` and `docker inspect`):

```sh
echo 'DBSNAP_PASSWORD=secret' | sudo tee /volume1/docker/dbsnap/dbsnap.env >/dev/null
sudo chmod 600 /volume1/docker/dbsnap/dbsnap.env   # root-only
```

Find docker's absolute path — cron runs with a minimal `PATH`:

```sh
command -v docker   # commonly /usr/bin/docker
```

Create the wrapper script. The DB name is an argument, so one script backs
up any database:

```sh
# writes everything up to EOF into the script file
sudo tee /volume1/docker/dbsnap/run-backup.sh >/dev/null <<'EOF'
#!/bin/sh
DB="${1:?usage: run-backup.sh <db-name>}"   # DB name from the first argument
DOCKER=/usr/bin/docker                      # set to your `command -v docker`
LOG=/volume1/docker/dbsnap/backup.log       # run output is appended here
ts() { date '+%Y-%m-%d %H:%M:%S'; }

echo "===== $(ts) starting $DB =====" >> "$LOG"
"$DOCKER" run --rm \
  --env-file /volume1/docker/dbsnap/dbsnap.env \
  -v /volume1/docker/dbsnap/backups:/data/backups \
  dbsnap backup -host 192.168.1.50 -user postgres -db "$DB" \
    -out /data/backups -non-interactive >> "$LOG" 2>&1
status=$?

# prune dumps older than 50 days, but only after a successful run so a
# broken backup can't delete your last good copies
if [ "$status" -eq 0 ]; then
  find /volume1/docker/dbsnap/backups -name '*.dump' -mtime +50 -delete
fi

echo "===== $(ts) finished $DB, exit $status =====" >> "$LOG"
exit $status                                # cron reads this exit code
EOF
```

Make it executable:

```sh
sudo chmod +x /volume1/docker/dbsnap/run-backup.sh
```

Edit two placeholders first — `-host` (your Postgres server) and `DOCKER=`
(the path from `command -v docker`). The script prunes dumps older than 50
days, but only after a successful run, so a failed backup never leaves you
with zero copies; change `+50` to adjust the window.

Test it — `echo $?` should print `0`; any other value means it failed, so
check `backup.log`:

```sh
sudo /volume1/docker/dbsnap/run-backup.sh db_name; echo $?
```

Confirm the dump landed and the run logged cleanly:

```sh
sudo ls -la /volume1/docker/dbsnap/backups      # a fresh, non-zero .dump
sudo tail -3 /volume1/docker/dbsnap/backup.log  # ...finished db_name, exit 0
```

Add it to root's crontab (root, to read the 0600 env file and reach the
Docker socket):

```sh
sudo crontab -e
```

Daily at 02:45, DB name as the argument:

```cron
45 2 * * * /volume1/docker/dbsnap/run-backup.sh db_name
```

Cron uses the NAS's local clock, so set the timezone to your own under
*Control Panel → Time & Language* — then 02:45 means 02:45 local time.
Verify with `sudo crontab -l`, and confirm a fresh dump the next morning.

## Reference

### pg_dump version

The `Dockerfile` installs `postgresql-client-18` from the PostgreSQL
[PGDG apt repo](https://wiki.postgresql.org/wiki/Apt), not Debian
bookworm's base client (version 15). `pg_dump` must be at least as new as
the server it dumps, or the backup fails with `server version: X;
pg_dump version: Y` and writes zero rows. Version 18 is backward compatible
and covers servers 15–18.

To back up a PostgreSQL 19+ server, bump `postgresql-client-18` in the
`Dockerfile` and re-ship the image. No Go changes needed — dbsnap calls
`pg_dump` from `PATH`, and the PGDG wrapper resolves whichever version is
installed.
