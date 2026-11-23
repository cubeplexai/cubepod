#!/usr/bin/env bash
# An OCI image is only a transport: never execute it and never accept mutable tags.
set -Eeuo pipefail
HERE=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
LOCK=${1:?usage: fetch.sh LOCK_JSON NEW_DEST_DIRECTORY}
DEST=${2:?usage: fetch.sh LOCK_JSON NEW_DEST_DIRECTORY}
metadata=$(python3 "$HERE/pin.py" read "$LOCK")
mapfile -t fields <<< "$metadata"
[[ ! -e "$DEST" && ! -L "$DEST" ]] || { echo 'Refusing existing kernel destination' >&2; exit 1; }
NAME="cubepod-guest-kernel-fetch-$$"
! docker inspect "$NAME" >/dev/null 2>&1 || { echo 'Refusing existing fetch container' >&2; exit 1; }
parent=$(dirname -- "$DEST"); base=$(basename -- "$DEST")
mkdir -p -- "$parent"
STAGE=$(mktemp -d "$parent/.$base.staging.XXXXXXXX")
cleanup() {
  docker rm -f "$NAME" >/dev/null 2>&1 || true
  # Only the unique directory allocated by mktemp, never the user's destination.
  [[ ! -d "$STAGE" ]] || rm -rf -- "$STAGE"
}
trap cleanup EXIT
# An omitted default command is required by docker create for scratch artifacts.
docker pull "${fields[0]}"
docker create --name "$NAME" --label io.cubepod.guest-kernel=fetch "${fields[0]}" /never-executed
docker cp "$NAME:/kernel/." "$STAGE/"
python3 "$HERE/pin.py" verify "$LOCK" "$STAGE"
cp "$LOCK" "$STAGE/pin.json"
# Publish only fully verified files; refuse even a destination created meanwhile.
mv -T -n -- "$STAGE" "$DEST"
[[ ! -d "$STAGE" ]] || { echo 'Destination appeared during verification; refusing overwrite' >&2; exit 1; }
