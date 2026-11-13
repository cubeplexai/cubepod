#!/bin/sh
set -eu

EGRESS_UID="${CUBEPOD_EGRESS_UID:-65532}"
TRANSPARENT_HTTP_PORT="${CUBEPOD_TRANSPARENT_HTTP_PORT:-18081}"
TRANSPARENT_TLS_PORT="${CUBEPOD_TRANSPARENT_TLS_PORT:-18443}"
DNS_PORT="${CUBEPOD_DNS_PORT:-18053}"

if [ "${CUBEPOD_TRANSPARENT:-0}" = "1" ] || [ "${CUBEPOD_TRANSPARENT:-}" = "true" ]; then
  # Idempotently replace the cubepod_egress nftables table.
  nft delete table inet cubepod_egress 2>/dev/null || true

  nft -f - <<EOF
table inet cubepod_egress {
  chain output_nat {
    type nat hook output priority dstnat; policy accept;

    # Never intercept traffic from cubepod-egress itself or loopback traffic.
    meta skuid ${EGRESS_UID} return
    oifname "lo" return
    ip daddr 127.0.0.0/8 return
    ip6 daddr ::1 return

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

    # Block QUIC (UDP 443) so HTTP clients fall back to TCP TLS 1.2/1.3.
    udp dport 443 reject
  }
}
EOF
fi

if [ "$(id -u)" = "0" ]; then
  exec su-exec "${EGRESS_UID}:${EGRESS_UID}" /usr/local/bin/cubepod-egress "$@"
fi

exec /usr/local/bin/cubepod-egress "$@"
