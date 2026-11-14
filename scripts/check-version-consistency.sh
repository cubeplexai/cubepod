#!/usr/bin/env bash
# Verify that the release version is consistent across Cargo, Go, OpenAPI, Helm, and SDKs.
set -euo pipefail

if [[ $# -ne 1 || ! "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 v<semver>" >&2
  exit 2
fi

tag="$1"
expected="${tag#v}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

cargo_pkg=$(awk '
  /^\[workspace\.package\]/ { in_ws=1; next }
  /^\[/ { in_ws=0 }
  in_ws && /^version = "/ { gsub(/^version = "|"/, ""); print; exit }
' Cargo.toml)

cargo_lock_podd=$(awk '
  $0 == "name = \"podd\"" { found=1; next }
  found && /^version = "/ { gsub(/^version = "|"/, ""); print; exit }
' Cargo.lock)

cargo_lock_podd_init=$(awk '
  $0 == "name = \"podd-init\"" { found=1; next }
  found && /^version = "/ { gsub(/^version = "|"/, ""); print; exit }
' Cargo.lock)

go_version=$(sed -nE 's/^var Version = "v([^"]+)"$/\1/p' pkg/version/version.go | head -1)
openapi_version=$(sed -nE 's/^  version: ([^ ]+)$/\1/p' api/openapi/cubepod-v1.yaml | head -1)
controller_podd=$(sed -nE 's/.*ghcr\.io\/cubeplexai\/cubepod-podd:v([^"]+)".*/\1/p' cmd/cubepod-controller/main.go | head -1)
gateway_podd=$(sed -nE 's/.*ghcr\.io\/cubeplexai\/cubepod-podd:v([^"]+)".*/\1/p' cmd/cubepod-gateway/main.go | head -1)
chart_version=$(sed -nE 's/^version: ([^ ]+)$/\1/p' deploy/helm/cubepod/Chart.yaml | head -1)
chart_app=$(sed -nE 's/^appVersion: "([^"]+)"$/\1/p' deploy/helm/cubepod/Chart.yaml | head -1)
values_podd=$(sed -nE 's/^[[:space:]]*poddInjectorImage: "ghcr\.io\/cubeplexai\/cubepod-podd:v([^"]+)"$/\1/p' deploy/helm/cubepod/values.yaml | head -1)
values_gateway=$(awk '/^gateway:/{s=1} s && /tag:/{gsub(/.*tag: "v|"/, ""); print; exit}' deploy/helm/cubepod/values.yaml)
values_controller=$(awk '/^controller:/{s=1} s && /tag:/{gsub(/.*tag: "v|"/, ""); print; exit}' deploy/helm/cubepod/values.yaml)
values_egress=$(awk '/^egress:/{s=1} s && /tag:/{gsub(/.*tag: "v|"/, ""); print; exit}' deploy/helm/cubepod/values.yaml)
python_pyproject=$(awk '
  /^\[project\]/ { in_proj=1; next }
  /^\[/ { in_proj=0 }
  in_proj && /^version = "/ { gsub(/^version = "|"/, ""); print; exit }
' sdks/python/pyproject.toml)
python_init=$(sed -nE 's/^__version__ = "([^"]+)"$/\1/p' sdks/python/cubepod/__init__.py | head -1)
ts_pkg=$(node -p "require('./sdks/typescript/package.json').version")

declare -A versions=(
  [Cargo.toml]="$cargo_pkg"
  [Cargo.lock:podd]="$cargo_lock_podd"
  [Cargo.lock:podd-init]="$cargo_lock_podd_init"
  [pkg/version/version.go]="$go_version"
  [api/openapi/cubepod-v1.yaml]="$openapi_version"
  [cmd/cubepod-controller/main.go]="$controller_podd"
  [cmd/cubepod-gateway/main.go]="$gateway_podd"
  [deploy/helm/cubepod/Chart.yaml:version]="$chart_version"
  [deploy/helm/cubepod/Chart.yaml:appVersion]="$chart_app"
  [deploy/helm/cubepod/values.yaml:poddInjectorImage]="$values_podd"
  [deploy/helm/cubepod/values.yaml:gateway.image.tag]="$values_gateway"
  [deploy/helm/cubepod/values.yaml:controller.image.tag]="$values_controller"
  [deploy/helm/cubepod/values.yaml:egress.image.tag]="$values_egress"
  [sdks/python/pyproject.toml]="$python_pyproject"
  [sdks/python/cubepod/__init__.py]="$python_init"
  [sdks/typescript/package.json]="$ts_pkg"
)

failed=0
for name in $(printf '%s\n' "${!versions[@]}" | sort); do
  if [[ "${versions[$name]}" != "$expected" ]]; then
    echo "version mismatch: $name=${versions[$name]} expected=$expected" >&2
    failed=1
  fi
done

if [[ $failed -ne 0 ]]; then
  exit 1
fi

echo "all CubePod package, binary, Helm, and SDK versions match $expected"
