#!/bin/sh
# Generate a stable panel HMAC secret for local compose if CUBEPOD_PANEL_SECRET
# is unset. The file lives on the cubepod-state volume so restarts reuse it.
set -e
secret_file="${CUBEPOD_PANEL_SECRET_FILE:-/var/lib/cubepod/state/panel-secret}"
if [ -z "${CUBEPOD_PANEL_SECRET}" ]; then
  if [ ! -s "${secret_file}" ]; then
    mkdir -p "$(dirname "${secret_file}")"
    dd if=/dev/urandom bs=32 count=1 2>/dev/null | od -An -tx1 | tr -d " \n" > "${secret_file}"
    chmod 600 "${secret_file}"
  fi
  CUBEPOD_PANEL_SECRET="$(cat "${secret_file}")"
  export CUBEPOD_PANEL_SECRET
fi
exec /usr/local/bin/cubepod-gateway "$@"
