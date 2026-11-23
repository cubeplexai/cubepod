package egress

import (
	"net"
	"strconv"
	"strings"
)

// DefaultNFSPort is the TCP port used by cubepod-volume-nfs (CEP-0031).
const DefaultNFSPort = 2049

// NFSDestinationExemption is a destination-prefixed nftables exemption for
// kernel NFSv3 client sockets (skuid 0) to the cubepod-volume-nfs ClusterIP.
// It must not be scoped to the sandbox user UID (1000).
type NFSDestinationExemption struct {
	DestIP string
	Port   int
}

// ParseNFSAddr parses CUBEPOD_VOLUME_NFS_ADDR ("<clusterIP>:<port>" or "[ipv6]:<port>").
func ParseNFSAddr(addr string) (NFSDestinationExemption, bool) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return NFSDestinationExemption{}, false
	}
	if ip := net.ParseIP(strings.Trim(addr, "[]")); ip != nil {
		return NFSDestinationExemption{DestIP: ip.String(), Port: DefaultNFSPort}, true
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil || strings.TrimSpace(host) == "" {
		return NFSDestinationExemption{}, false
	}
	host = strings.Trim(host, "[]")
	ip := net.ParseIP(host)
	if ip == nil {
		return NFSDestinationExemption{}, false
	}
	port := DefaultNFSPort
	if strings.TrimSpace(portStr) != "" {
		p, err := strconv.Atoi(strings.TrimSpace(portStr))
		if err != nil || p <= 0 {
			return NFSDestinationExemption{}, false
		}
		port = p
	}
	return NFSDestinationExemption{DestIP: ip.String(), Port: port}, true
}

// NFSPortAllowed reports whether an outbound TCP connection to destIP:destPort
// is allowed by the volume NFS exemption and a default-deny policy for other
// destinations on the NFS port. skuid is accepted (including 0) when the
// destination matches the exemption; the rule is not UID-scoped to 1000.
func NFSPortAllowed(destIP string, destPort, skuid int, exemption *NFSDestinationExemption) bool {
	_ = skuid // kernel NFS uses skuid 0; exemption is destination-based.
	if exemption == nil || exemption.DestIP == "" {
		// No volume-NFS exemption is installed; egress-init.sh does not
		// reject other :2049 destinations either.
		return true
	}
	port := exemption.Port
	if port <= 0 {
		port = DefaultNFSPort
	}
	if destPort != port {
		return true
	}
	want := net.ParseIP(exemption.DestIP)
	got := net.ParseIP(destIP)
	if want != nil && got != nil {
		return want.Equal(got)
	}
	return destIP == exemption.DestIP
}
