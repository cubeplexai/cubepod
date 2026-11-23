#!/bin/sh
set -eu

EGRESS_UID="${CUBEPOD_EGRESS_UID:-65532}"
TRANSPARENT_HTTP_PORT="${CUBEPOD_TRANSPARENT_HTTP_PORT:-18081}"
TRANSPARENT_TLS_PORT="${CUBEPOD_TRANSPARENT_TLS_PORT:-18443}"
DNS_PORT="${CUBEPOD_DNS_PORT:-18053}"
VOLUME_NFS_ADDR="${CUBEPOD_VOLUME_NFS_ADDR:-}"

if [ "${CUBEPOD_TRANSPARENT:-0}" = "1" ] || [ "${CUBEPOD_TRANSPARENT:-}" = "true" ]; then
  # Idempotently replace the cubepod_egress nftables table.
  nft delete table inet cubepod_egress 2>/dev/null || true

  NFS_NAT_RULE=""
  NFS_FILTER_ALLOW=""
  NFS_FILTER_DENY=""
  if [ -n "${VOLUME_NFS_ADDR}" ]; then
    NFS_FAMILY="ip"
    NFS_IP=""
    NFS_PORT="2049"
    case "${VOLUME_NFS_ADDR}" in
      \[*)
        NFS_FAMILY="ip6"
        NFS_IP="${VOLUME_NFS_ADDR#\[}"
        NFS_REST="${NFS_IP#*\]}"
        NFS_IP="${NFS_IP%%\]*}"
        if [ "${NFS_REST}" != "${NFS_REST#:}" ]; then
          NFS_PORT="${NFS_REST#:}"
        fi
        ;;
      *)
        NFS_IP="${VOLUME_NFS_ADDR%:*}"
        NFS_PORT="${VOLUME_NFS_ADDR##*:}"
        if [ -z "${NFS_PORT}" ] || [ "${NFS_PORT}" = "${VOLUME_NFS_ADDR}" ]; then
          NFS_PORT="2049"
        fi
        case "${NFS_IP}" in
          *:*) NFS_FAMILY="ip6" ;;
        esac
        ;;
    esac
    if [ -z "${NFS_PORT}" ]; then
      NFS_PORT="2049"
    fi
    # Destination-prefixed exemption for kernel NFS (skuid 0) to the volume NFS Service.
    # Do not scope this to the sandbox UID; rpciod sockets originate as root.
    # IPv6 ClusterIPs use ip6 daddr; IPv4 uses ip daddr.
    NFS_NAT_RULE="    ${NFS_FAMILY} daddr ${NFS_IP} tcp dport ${NFS_PORT} return"
    NFS_FILTER_ALLOW="    ${NFS_FAMILY} daddr ${NFS_IP} tcp dport ${NFS_PORT} accept"
    # Reject other :nfs-port destinations so DefaultAction=Deny is honored in nftables,
    # not only in the HTTP/HTTPS proxy.
    NFS_FILTER_DENY="    tcp dport ${NFS_PORT} reject"
  fi

  nft -f - <<NFT
table inet cubepod_egress {
  chain output_nat {
    type nat hook output priority dstnat; policy accept;

    # Never intercept traffic from cubepod-egress itself or loopback traffic.
    meta skuid ${EGRESS_UID} return
    oifname "lo" return
    ip daddr 127.0.0.0/8 return
    ip6 daddr ::1 return
${NFS_NAT_RULE}

    # Redirect sandbox DNS and HTTP/HTTPS traffic to cubepod-egress listeners.
    udp dport 53 redirect to :${DNS_PORT}
    tcp dport 53 redirect to :${DNS_PORT}
    tcp dport 80 redirect to :${TRANSPARENT_HTTP_PORT}
    tcp dport 443 redirect to :${TRANSPARENT_TLS_PORT}
  }

  chain output_filter {
    type filter hook output priority filter; policy accept;

    meta skuid ${EGRESS_UID} accept
    oifname "lo" accept
    ct state established,related accept
${NFS_FILTER_ALLOW}
${NFS_FILTER_DENY}

    # Block QUIC (UDP 443) so HTTP clients fall back to TCP TLS 1.2/1.3.
    udp dport 443 reject
  }
}
NFT
fi

if [ "$(id -u)" = "0" ]; then
  exec su-exec "${EGRESS_UID}:${EGRESS_UID}" /usr/local/bin/cubepod-egress "$@"
fi

exec /usr/local/bin/cubepod-egress "$@"
