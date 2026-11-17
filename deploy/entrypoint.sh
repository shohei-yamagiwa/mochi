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
  mochi validate --runtime
  exec mochi serve \
    --addr ":${PORT:-8080}" \
    --db "${MOCHI_DB_PATH:-/data/mochi.db}" \
    --workers "${MOCHI_WORKERS:-1}" \
    --queue-size "${MOCHI_QUEUE_SIZE:-16}" \
    "$@"
fi

exec mochi "$@"
