package egress

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNFSPortAllowedDestinationExemptionAndDefaultDeny(t *testing.T) {
	ex := &NFSDestinationExemption{DestIP: "10.96.10.20", Port: 2049}

	if !NFSPortAllowed("10.96.10.20", 2049, 0, ex) {
		t.Fatal("skuid 0 to ClusterIP:2049 must be allowed")
	}
	if !NFSPortAllowed("10.96.10.20", 2049, 1000, ex) {
		t.Fatal("sandbox UID to ClusterIP:2049 must be allowed")
	}
	if NFSPortAllowed("8.8.8.8", 2049, 0, ex) {
		t.Fatal("other :2049 destinations must be denied")
	}
	if NFSPortAllowed("8.8.8.8", 2049, 1000, ex) {
		t.Fatal("other :2049 destinations must be denied for sandbox UID too")
	}
	if !NFSPortAllowed("8.8.8.8", 443, 1000, ex) {
		t.Fatal("non-NFS ports are out of scope for the NFS port reject")
	}
}

func TestParseNFSAddr(t *testing.T) {
	ex, ok := ParseNFSAddr("10.96.10.20:2049")
	if !ok || ex.DestIP != "10.96.10.20" || ex.Port != 2049 {
		t.Fatalf("got %+v ok=%v", ex, ok)
	}
	if _, ok := ParseNFSAddr(""); ok {
		t.Fatal("empty addr")
	}
}

func TestEgressInitScriptInstallsDestinationExemptionAndRejectsOther2049(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	script := filepath.Join(filepath.Dir(file), "..", "..", "images", "egress", "egress-init.sh")
	body, err := os.ReadFile(script)
	if err != nil {
		t.Fatal(err)
	}
	src := string(body)
	for _, want := range []string{
		"CUBEPOD_VOLUME_NFS_ADDR",
		"${NFS_FAMILY} daddr ${NFS_IP} tcp dport ${NFS_PORT} return",
		"${NFS_FAMILY} daddr ${NFS_IP} tcp dport ${NFS_PORT} accept",
		"tcp dport ${NFS_PORT} reject",
	} {
		if !strings.Contains(src, want) {
			t.Fatalf("egress-init.sh missing %q", want)
		}
	}
	acceptIdx := strings.Index(src, "${NFS_FAMILY} daddr ${NFS_IP} tcp dport ${NFS_PORT} accept")
	if acceptIdx < 0 {
		t.Fatal("missing accept rule")
	}
	window := src[max(0, acceptIdx-80) : acceptIdx+80]
	if strings.Contains(window, "skuid 1000") {
		t.Fatal("NFS exemption must not be scoped to sandbox UID 1000")
	}
}

func TestParseNFSAddrIPv6AndHostPort(t *testing.T) {
	got, ok := ParseNFSAddr("[2001:db8::10]:2049")
	if !ok || got.DestIP != "2001:db8::10" || got.Port != 2049 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	got, ok = ParseNFSAddr("10.96.10.20:2050")
	if !ok || got.DestIP != "10.96.10.20" || got.Port != 2050 {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
	if !NFSPortAllowed("2001:db8::10", 2049, 0, &NFSDestinationExemption{DestIP: "2001:db8::10", Port: 2049}) {
		t.Fatal("ipv6 exemption should allow skuid 0")
	}
	if NFSPortAllowed("2001:db8::11", 2049, 0, &NFSDestinationExemption{DestIP: "2001:db8::10", Port: 2049}) {
		t.Fatal("other ipv6 :2049 destinations must be denied")
	}
}

func TestNFSPortAllowedWithoutExemptionAllows2049(t *testing.T) {
	if !NFSPortAllowed("8.8.8.8", 2049, 0, nil) {
		t.Fatal("when CUBEPOD_VOLUME_NFS_ADDR is unset, :2049 must not be rejected")
	}
}
