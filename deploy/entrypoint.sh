#!/bin/sh
set -eu
umask 077

if [ "$(id -u)" = 0 ]; then
  mkdir -p /data
  chown mochi:mochi /data
  exec su-exec mochi:mochi "$0" "$@"
fi

if [ "${1:-}" = serve ]; then
  shift
  workflow=${MOCHI_WORKFLOW:-configs/workflows/pr-review.yaml}
  mochi validate --runtime "$workflow"
  exec mochi serve \
    --addr ":${PORT:-8080}" \
    --db "${MOCHI_DB_PATH:-/data/mochi.db}" \
    --workflow "$workflow" \
    --base-dir /app \
    --workers "${MOCHI_WORKERS:-1}" \
    --queue-size "${MOCHI_QUEUE_SIZE:-16}" \
    "$@"
fi

exec mochi "$@"
