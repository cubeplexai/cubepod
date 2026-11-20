#!/bin/sh
# Generate a stable panel HMAC secret for local compose if CUBEPOD_PANEL_SECRET
# is unset. The file lives on the cubepod-state volume so restarts reuse it.
set -eu
umask 077
secret_file="${CUBEPOD_PANEL_SECRET_FILE:-/var/lib/cubepod/state/panel-secret}"
if [ -z "${CUBEPOD_PANEL_SECRET:-}" ]; then
  if [ ! -e "${secret_file}" ]; then
    mkdir -p "$(dirname "${secret_file}")"
    # Check the random source separately: POSIX sh has no pipefail.
    secret="$(od -An -N32 -tx1 /dev/urandom)"
    secret="$(printf '%s' "$secret" | tr -d ' \n')"
    case "$secret" in
      *[!0-9a-f]*|'') echo "failed to generate panel secret" >&2; exit 1 ;;
    esac
    [ "${#secret}" -eq 64 ] || { echo "short panel secret" >&2; exit 1; }
    tmp="$(mktemp "${secret_file}.XXXXXX")"
    trap 'rm -f "$tmp"' EXIT
    trap 'exit 1' HUP INT TERM
    printf '%s\n' "$secret" > "$tmp"
    # A hard link atomically publishes the complete 0600 file without
    # replacing a key another first-start process already published.
    if ! ln "$tmp" "$secret_file" 2>/dev/null; then
      [ -s "$secret_file" ] || { echo "cannot publish panel secret" >&2; exit 1; }
    fi
    rm -f "$tmp"
    trap - EXIT HUP INT TERM
  fi
  [ -s "$secret_file" ] || { echo "panel secret file is empty" >&2; exit 1; }
  CUBEPOD_PANEL_SECRET="$(cat "$secret_file")"
  export CUBEPOD_PANEL_SECRET
fi

# Compose overlays may still pass the binary as command[0], from before this
# entrypoint existed. Go's flag parser stops at the first non-flag, so a
# duplicated path would drop --e2b-template-api=false and other options.
gateway="/usr/local/bin/cubepod-gateway"
command0="${1:-}"
if [ "$command0" = "$gateway" ] || [ "${command0##*/}" = "cubepod-gateway" ]; then
  exec "$@"
fi
exec "${gateway}" "$@"
