#!/bin/bash
set -euo pipefail

# Ships the dbsnap Docker image to a UGREEN NAS.
# Builds the image locally for the target platform and loads it into the
# NAS's Docker via SSH (no registry) — dbsnap is a CLI, not a long-running
# service, so there's nothing to start here; run it with `docker run` on
# the NAS afterwards (see docs/ugreen-nas.md).
#
# Requires: Docker on the NAS (see docs/ugreen-nas.md for first-time setup).
#
# Make executable: chmod +x ./scripts/deploy-nas.sh
#
# Run from the repo root:
#   ./scripts/deploy-nas.sh --host admin@nas.local

usage() {
  echo "Usage: ./scripts/deploy-nas.sh --host <user@host> [--platform <linux/amd64|linux/arm64>]"
  exit 1
}

PLATFORM="linux/amd64"

while [[ $# -gt 0 ]]; do
  case $1 in
    --host)
      REMOTE_HOST="$2"; shift 2
      ;;
    --platform)
      PLATFORM="$2"; shift 2
      ;;
    *) echo "Unknown parameter: $1"; usage ;;
  esac
done

if [[ -z "${REMOTE_HOST:-}" ]]; then
  echo "Error: --host is required."
  usage
fi

SSH_OPTS=(-o ConnectTimeout=5 -o BatchMode=yes)

# Pre-flight: host reachable?
if ! ssh "${SSH_OPTS[@]}" "$REMOTE_HOST" true 2>/dev/null; then
  echo "Error: Cannot reach $REMOTE_HOST over SSH."
  exit 1
fi

# Pre-flight: Docker running on the NAS?
if ! ssh "${SSH_OPTS[@]}" "$REMOTE_HOST" "docker version" >/dev/null 2>&1; then
  echo "Error: Docker isn't reachable on $REMOTE_HOST. Install the Docker app first (see docs/ugreen-nas.md)."
  exit 1
fi

GIT_COMMIT=$(git rev-parse --short HEAD)
IMAGE_TAG="dbsnap:${GIT_COMMIT}"

echo ""
echo "========================================="
echo "  dbsnap — NAS Deployment Summary"
echo "========================================="
echo "  Host     : $REMOTE_HOST"
echo "  Platform : $PLATFORM"
echo "  Commit   : $GIT_COMMIT"
echo "  Image    : $IMAGE_TAG"
echo "========================================="
echo ""

# Build the image locally for the target platform.
# --load brings the result into the local Docker image store so we can save it.
echo "==> Building image for $PLATFORM..."
docker buildx build \
  --platform "$PLATFORM" \
  -t "$IMAGE_TAG" \
  -t dbsnap:latest \
  --load \
  .

# Ship the image to the NAS as a gzipped tar over SSH.
echo "==> Shipping image to $REMOTE_HOST..."
docker save "$IMAGE_TAG" dbsnap:latest | gzip | ssh "$REMOTE_HOST" 'gunzip | docker load'

echo ""
echo "========================================="
echo "  Done — image loaded on $REMOTE_HOST"
echo "========================================="
echo "  Run it with, e.g.:"
echo "    docker run --rm -it -e DBSNAP_PASSWORD=secret \\"
echo "      -v /volume1/docker/dbsnap/backups:/data/backups \\"
echo "      dbsnap backup -host <pg-host> -user postgres -db appdb -out /data/backups"
echo "========================================="
echo ""
