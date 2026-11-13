#!/usr/bin/env bash
# Idempotent host preparation and diagnostic script for Kata Containers on containerd.
# Ensures /dev/kvm readiness and sets:
#   privileged_without_host_devices = true
#   privileged_without_host_devices_all_devices_allowed = true
# in containerd Kata runtime stanzas (and/or Kata configuration-*.toml) per
# docs/architecture.md §3.3 to prevent `/dev/full EEXIST` on privileged Block CSI pods.
set -euo pipefail

DRY_RUN=0
ALLOW_MISSING_KVM=0
RESTART_CONTAINERD=0
CONFIG_PATH="${CONTAINERD_CONFIG_PATH:-/etc/containerd/config.toml}"
KATA_CONFIG_PATH="${KATA_CONFIG_PATH:-}"

usage() {
  cat <<'EOF'
Usage: configure-containerd-kata.sh [OPTIONS]

Options:
  --dry-run                  Print diagnostics and planned config diff without modifying files.
  --config-path <path>       Path to containerd config.toml or Kata configuration-*.toml
                             (default: /etc/containerd/config.toml).
  --kata-config-path <path>  Optional Kata hypervisor TOML (e.g. configuration-qemu.toml) to also check/update.
  --allow-missing-kvm        Do not fail if /dev/kvm is absent (dry-run always warns only).
  --restart                  Restart containerd via systemctl when changes are applied.
  -h, --help                 Show this help message.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run)
      DRY_RUN=1
      shift
      ;;
    --config-path)
      if [[ $# -lt 2 ]]; then
        echo "ERROR: --config-path requires an argument" >&2
        exit 2
      fi
      CONFIG_PATH="$2"
      shift 2
      ;;
    --config-path=*)
      CONFIG_PATH="${1#*=}"
      shift
      ;;
    --kata-config-path)
      if [[ $# -lt 2 ]]; then
        echo "ERROR: --kata-config-path requires an argument" >&2
        exit 2
      fi
      KATA_CONFIG_PATH="$2"
      shift 2
      ;;
    --kata-config-path=*)
      KATA_CONFIG_PATH="${1#*=}"
      shift
      ;;
    --allow-missing-kvm)
      ALLOW_MISSING_KVM=1
      shift
      ;;
    --restart)
      RESTART_CONTAINERD=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "ERROR: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

echo "=== CubePod Kata + containerd Diagnostic & Configuration ==="
echo "[INFO] mode=$([[ "$DRY_RUN" -eq 1 ]] && echo dry-run || echo apply) config_path=${CONFIG_PATH}"

# 1. Verify /dev/kvm and host virtualization readiness.
KVM_OK=0
if [[ -e /dev/kvm ]]; then
  if [[ -c /dev/kvm ]]; then
    if [[ -r /dev/kvm && -w /dev/kvm ]]; then
      echo "[OK]   /dev/kvm exists, is a character device, and is read/write accessible"
      KVM_OK=1
    else
      echo "[WARN] /dev/kvm exists as character device but current user lacks rw permissions ($(ls -l /dev/kvm 2>/dev/null || true))"
      KVM_OK=1
    fi
  else
    echo "[WARN] /dev/kvm exists but is not a character device"
  fi
else
  echo "[WARN] /dev/kvm not found; hardware virtualization (KVM) is required for Kata MicroVMs in production"
fi

if [[ -r /proc/cpuinfo ]]; then
  if grep -qE '\b(vmx|svm)\b' /proc/cpuinfo 2>/dev/null; then
    echo "[OK]   CPU virtualization extensions (vmx/svm) detected in /proc/cpuinfo"
  else
    echo "[WARN] CPU virtualization extensions (vmx/svm) not visible in /proc/cpuinfo (check BIOS/nested virt)"
  fi
fi

if [[ -d /sys/module/kvm ]]; then
  echo "[OK]   kernel module kvm is loaded"
else
  echo "[INFO] /sys/module/kvm not present"
fi

# 2. Check Kata / containerd binaries for diagnostics.
for bin in containerd containerd-shim-kata-v2 kata-runtime; do
  if found="$(command -v "$bin" 2>/dev/null)"; then
    echo "[OK]   binary ${bin}: ${found}"
  elif [[ -x "/opt/kata/bin/${bin}" ]]; then
    echo "[OK]   binary ${bin}: /opt/kata/bin/${bin}"
  else
    echo "[INFO] binary ${bin}: not found in PATH or /opt/kata/bin"
  fi
done

if [[ "$KVM_OK" -eq 0 && "$DRY_RUN" -eq 0 && "$ALLOW_MISSING_KVM" -eq 0 ]]; then
  echo "[ERROR] /dev/kvm is required when applying live Kata configuration (pass --allow-missing-kvm or --dry-run to override)" >&2
  exit 1
fi

# Discover default Kata config if not explicitly provided.
discover_kata_config() {
  local candidate
  for candidate in \
    /etc/kata-containers/configuration-qemu.toml \
    /opt/kata/share/defaults/kata-containers/configuration-qemu.toml \
    /usr/share/defaults/kata-containers/configuration-qemu.toml; do
    if [[ -f "$candidate" ]]; then
      echo "$candidate"
      return 0
    fi
  done
  return 1
}

# Detect whether a TOML file is a Kata configuration-*.toml vs containerd config.toml.
is_kata_toml() {
  local file="$1"
  local base
  base="$(basename "$file")"
  if [[ "$base" == configuration*.toml ]]; then
    return 0
  fi
  if grep -qE '^\[hypervisor\.|^\[runtime\]' "$file" 2>/dev/null && ! grep -q 'containerd\.runtimes' "$file" 2>/dev/null; then
    return 0
  fi
  return 1
}

# Render updated Kata configuration-*.toml with [runtime] privileged_without_host_devices flags.
render_kata_toml() {
  local src="$1"
  awk '
    function flush_runtime() {
      if (in_runtime) {
        if (!seen_pwhd) {
          print "privileged_without_host_devices = true"
        }
        if (!seen_pwhd_all) {
          print "privileged_without_host_devices_all_devices_allowed = true"
        }
      }
    }
    BEGIN {
      in_runtime = 0
      found_runtime = 0
      seen_pwhd = 0
      seen_pwhd_all = 0
    }
    /^[[:space:]]*\[/ {
      if (in_runtime) {
        flush_runtime()
        in_runtime = 0
      }
      if ($0 ~ /^[[:space:]]*\[runtime\][[:space:]]*(#.*)?$/) {
        in_runtime = 1
        found_runtime = 1
        seen_pwhd = 0
        seen_pwhd_all = 0
      }
      print $0
      next
    }
    {
      if (in_runtime) {
        if ($0 ~ /^[[:space:]]*#?[[:space:]]*privileged_without_host_devices[[:space:]]*=/) {
          if (!seen_pwhd) {
            print "privileged_without_host_devices = true"
            seen_pwhd = 1
          }
          next
        }
        if ($0 ~ /^[[:space:]]*#?[[:space:]]*privileged_without_host_devices_all_devices_allowed[[:space:]]*=/) {
          if (!seen_pwhd_all) {
            print "privileged_without_host_devices_all_devices_allowed = true"
            seen_pwhd_all = 1
          }
          next
        }
      }
      print $0
    }
    END {
      if (in_runtime) {
        flush_runtime()
      } else if (!found_runtime) {
        print ""
        print "[runtime]"
        print "privileged_without_host_devices = true"
        print "privileged_without_host_devices_all_devices_allowed = true"
      }
    }
  ' "$src"
}

# Render updated containerd config.toml ensuring all kata* runtime stanzas set both flags.
render_containerd_toml() {
  local src="$1"
  local default_qemu_cfg="/opt/kata/share/defaults/kata-containers/configuration-qemu.toml"
  local default_clh_cfg="/opt/kata/share/defaults/kata-containers/configuration-clh.toml"
  if [[ -n "$KATA_CONFIG_PATH" ]]; then
    default_qemu_cfg="$KATA_CONFIG_PATH"
  elif discovered="$(discover_kata_config 2>/dev/null || true)" && [[ -n "$discovered" ]]; then
    default_qemu_cfg="$discovered"
  fi

  local cri_prefix='plugins."io.containerd.grpc.v1.cri"'
  if grep -qE '^[[:space:]]*version[[:space:]]*=[[:space:]]*3([[:space:]]|$)' "$src" 2>/dev/null || \
     grep -q 'io\.containerd\.cri\.v1\.runtime' "$src" 2>/dev/null; then
    cri_prefix='plugins."io.containerd.cri.v1.runtime"'
  fi

  awk -v cri_prefix="$cri_prefix" -v qemu_cfg="$default_qemu_cfg" -v clh_cfg="$default_clh_cfg" '
    function flush_kata_section() {
      if (in_kata) {
        if (!seen_pwhd) {
          print indent "privileged_without_host_devices = true"
        }
        if (!seen_pwhd_all) {
          print indent "privileged_without_host_devices_all_devices_allowed = true"
        }
      }
    }
    BEGIN {
      in_kata = 0
      found_any_kata = 0
      seen_pwhd = 0
      seen_pwhd_all = 0
      indent = "  "
    }
    /^[[:space:]]*\[/ {
      if (in_kata) {
        flush_kata_section()
        in_kata = 0
      }
      # Match runtime table headers like:
      # [plugins."io.containerd.grpc.v1.cri".containerd.runtimes.kata-qemu]
      # [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.kata]
      # Exclude .options sub-tables.
      if ($0 ~ /^[[:space:]]*\[plugins\.[^]]*\.containerd\.runtimes\.kata[^.]*\][[:space:]]*(#.*)?$/) {
        in_kata = 1
        found_any_kata = 1
        seen_pwhd = 0
        seen_pwhd_all = 0
        match($0, /^[[:space:]]*/)
        base_indent = substr($0, RSTART, RLENGTH)
        indent = base_indent "  "
      }
      print $0
      next
    }
    {
      if (in_kata) {
        if ($0 ~ /^[[:space:]]*#?[[:space:]]*privileged_without_host_devices[[:space:]]*=/) {
          if (!seen_pwhd) {
            print indent "privileged_without_host_devices = true"
            seen_pwhd = 1
          }
          next
        }
        if ($0 ~ /^[[:space:]]*#?[[:space:]]*privileged_without_host_devices_all_devices_allowed[[:space:]]*=/) {
          if (!seen_pwhd_all) {
            print indent "privileged_without_host_devices_all_devices_allowed = true"
            seen_pwhd_all = 1
          }
          next
        }
      }
      print $0
    }
    END {
      if (in_kata) {
        flush_kata_section()
      }
      if (!found_any_kata) {
        print ""
        print "[" cri_prefix ".containerd.runtimes.kata-qemu]"
        print "  runtime_type = \"io.containerd.kata.v2\""
        print "  privileged_without_host_devices = true"
        print "  privileged_without_host_devices_all_devices_allowed = true"
        print "  [" cri_prefix ".containerd.runtimes.kata-qemu.options]"
        print "    ConfigPath = \"" qemu_cfg "\""
        print ""
        print "[" cri_prefix ".containerd.runtimes.kata-clh]"
        print "  runtime_type = \"io.containerd.kata.v2\""
        print "  privileged_without_host_devices = true"
        print "  privileged_without_host_devices_all_devices_allowed = true"
        print "  [" cri_prefix ".containerd.runtimes.kata-clh.options]"
        print "    ConfigPath = \"" clh_cfg "\""
      }
    }
  ' "$src"
}

CHANGES_NEEDED=0
CHANGES_APPLIED=0

process_toml_file() {
  local target="$1"
  local tmp_in tmp_out
  tmp_in="$(mktemp)"
  tmp_out="$(mktemp)"
  trap 'rm -f "$tmp_in" "$tmp_out"' RETURN

  if [[ -f "$target" ]]; then
    cp "$target" "$tmp_in"
  else
    if [[ "$DRY_RUN" -eq 0 ]]; then
      echo "[INFO] ${target} does not exist yet; will initialize new config"
    else
      echo "[DRY-RUN] ${target} does not exist; would initialize new config"
    fi
    : > "$tmp_in"
  fi

  if is_kata_toml "$target" || { [[ -s "$tmp_in" ]] && is_kata_toml "$tmp_in"; }; then
    echo "[INFO] ${target}: detected Kata runtime/hypervisor TOML format"
    render_kata_toml "$tmp_in" > "$tmp_out"
  else
    echo "[INFO] ${target}: detected containerd TOML format"
    render_containerd_toml "$tmp_in" > "$tmp_out"
  fi

  if [[ -f "$target" ]] && cmp -s "$tmp_in" "$tmp_out"; then
    echo "[OK]   ${target}: privileged_without_host_devices and privileged_without_host_devices_all_devices_allowed are already true (idempotent)"
    return 0
  fi

  CHANGES_NEEDED=1
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "[DRY-RUN] ${target}: planned changes:"
    diff -u --label "${target} (current)" --label "${target} (planned)" "$tmp_in" "$tmp_out" || true
    return 0
  fi

  mkdir -p "$(dirname "$target")"
  if [[ -f "$target" ]]; then
    local backup="${target}.bak.$(date +%Y%m%d%H%M%S)"
    cp "$target" "$backup"
    echo "[INFO] backup saved to ${backup}"
  fi
  cp "$tmp_out" "$target"
  CHANGES_APPLIED=1
  echo "[OK]   updated ${target}"

  if grep -q 'privileged_without_host_devices = true' "$target" && \
     grep -q 'privileged_without_host_devices_all_devices_allowed = true' "$target"; then
    echo "[OK]   verified required Kata device flags in ${target}"
  else
    echo "[ERROR] verification failed for ${target}" >&2
    exit 1
  fi
}

process_toml_file "$CONFIG_PATH"

if [[ -n "$KATA_CONFIG_PATH" && "$KATA_CONFIG_PATH" != "$CONFIG_PATH" ]]; then
  process_toml_file "$KATA_CONFIG_PATH"
elif [[ -z "$KATA_CONFIG_PATH" ]]; then
  if auto_kata="$(discover_kata_config 2>/dev/null || true)" && [[ -n "$auto_kata" && "$auto_kata" != "$CONFIG_PATH" ]]; then
    echo "[INFO] discovered Kata hypervisor config: ${auto_kata}"
    if grep -q 'privileged_without_host_devices' "$auto_kata" 2>/dev/null; then
      process_toml_file "$auto_kata"
    else
      echo "[INFO] ${auto_kata} defers privileged_without_host_devices to containerd config.toml"
    fi
  fi
fi

if [[ "$RESTART_CONTAINERD" -eq 1 ]]; then
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "[DRY-RUN] would run: systemctl restart containerd"
  elif [[ "$CHANGES_APPLIED" -eq 1 ]]; then
    echo "[INFO] restarting containerd..."
    systemctl restart containerd
    echo "[OK]   containerd restarted"
  else
    echo "[INFO] no config changes applied; skipping containerd restart"
  fi
fi

echo "=== Summary ==="
if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "[OK]   dry-run complete (changes_needed=${CHANGES_NEEDED}, files_modified=0)"
else
  echo "[OK]   configuration complete (changes_applied=${CHANGES_APPLIED})"
fi
