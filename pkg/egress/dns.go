package egress

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cubeplexai/cubepod/api/v1alpha1"
)

const (
	dnsHeaderLen     = 12
	dnsRcodeFormErr  = 1
	dnsRcodeServFail = 2
	dnsRcodeRefused  = 5
)

// DNSConfig configures the policy-enforcing DNS forwarder.
type DNSConfig struct {
	UpstreamAddr string
	GetPolicy    func() v1alpha1.NetworkPolicySpec
	Metrics      *Metrics
	Timeout      time.Duration
}

// DNSServer is a pure-stdlib UDP/TCP DNS allowlist proxy that returns RCODE=5 (Refused)
// for domains blocked by NetworkPolicySpec and forwards allowed queries upstream.
type DNSServer struct {
	upstreamAddr string
	getPolicy    func() v1alpha1.NetworkPolicySpec
	metrics      *Metrics
	timeout      time.Duration
}

// NewDNSServer constructs a DNSServer.
func NewDNSServer(cfg DNSConfig) *DNSServer {
	upstream := strings.TrimSpace(cfg.UpstreamAddr)
	if upstream == "" {
		upstream = defaultUpstreamDNS()
	} else if !strings.Contains(upstream, ":") {
		upstream = net.JoinHostPort(upstream, "53")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	getPolicy := cfg.GetPolicy
	if getPolicy == nil {
		getPolicy = func() v1alpha1.NetworkPolicySpec {
			return v1alpha1.NetworkPolicySpec{DefaultAction: v1alpha1.NetworkDefaultDeny}
		}
	}

	return &DNSServer{
		upstreamAddr: upstream,
		getPolicy:    getPolicy,
		metrics:      cfg.Metrics,
		timeout:      timeout,
	}
}

func defaultUpstreamDNS() string {
	data, err := os.ReadFile("/etc/resolv.conf")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) >= 2 && fields[0] == "nameserver" {
				ip := net.ParseIP(fields[1])
				if ip != nil && !ip.IsLoopback() {
					return net.JoinHostPort(ip.String(), "53")
				}
			}
		}
	}
	return "8.8.8.8:53"
}

// ListenAndServe starts both UDP and TCP DNS listeners on addr and blocks until an error occurs.
func (s *DNSServer) ListenAndServe(addr string) error {
	pc, err := net.ListenPacket("udp", addr)
	if err != nil {
		return fmt.Errorf("dns udp listen %q: %w", addr, err)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		_ = pc.Close()
		return fmt.Errorf("dns tcp listen %q: %w", addr, err)
	}

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		errCh <- s.ServePacketConn(pc)
	}()
	go func() {
		defer wg.Done()
		errCh <- s.ServeTCPListener(ln)
	}()

	err = <-errCh
	_ = pc.Close()
	_ = ln.Close()
	wg.Wait()
	return err
}

// ServePacketConn serves UDP DNS queries on pc until pc is closed.
func (s *DNSServer) ServePacketConn(pc net.PacketConn) error {
	buf := make([]byte, 4096)
	for {
		n, peerAddr, err := pc.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		query := make([]byte, n)
		copy(query, buf[:n])

		go func(q []byte, addr net.Addr) {
			resp, err := s.handleQuery(q, false)
			if err != nil || len(resp) == 0 {
				return
			}
			_, _ = pc.WriteTo(resp, addr)
		}(query, peerAddr)
	}
}

// ServeTCPListener serves TCP DNS queries on ln until ln is closed.
func (s *DNSServer) ServeTCPListener(ln net.Listener) error {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.serveTCPConn(conn)
	}
}

func (s *DNSServer) serveTCPConn(conn net.Conn) {
	defer conn.Close()
	for {
		_ = conn.SetReadDeadline(time.Now().Add(s.timeout))
		var lenBuf [2]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return
		}
		msgLen := int(binary.BigEndian.Uint16(lenBuf[:]))
		if msgLen < dnsHeaderLen || msgLen > 65535 {
			return
		}
		query := make([]byte, msgLen)
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}

		resp, err := s.handleQuery(query, true)
		if err != nil || len(resp) == 0 {
			return
		}
		var outLen [2]byte
		binary.BigEndian.PutUint16(outLen[:], uint16(len(resp)))
		if _, err := conn.Write(outLen[:]); err != nil {
			return
		}
		if _, err := conn.Write(resp); err != nil {
			return
		}
	}
}

// HandleDNSQuery evaluates policy for a single raw DNS query packet and either returns
// RCODE=5 (Refused) or forwards it to the upstream DNS resolver over UDP.
func (s *DNSServer) HandleDNSQuery(query []byte) ([]byte, error) {
	return s.handleQuery(query, false)
}

func (s *DNSServer) handleQuery(query []byte, useTCP bool) ([]byte, error) {
	if len(query) < dnsHeaderLen {
		return nil, errors.New("dns packet shorter than header")
	}

	qdCount := binary.BigEndian.Uint16(query[4:6])
	if qdCount == 0 {
		return buildDNSErrorResponse(query, dnsHeaderLen, dnsRcodeFormErr), nil
	}

	qname, questionEnd, err := parseDNSQName(query, dnsHeaderLen)
	if err != nil {
		return buildDNSErrorResponse(query, dnsHeaderLen, dnsRcodeFormErr), nil
	}

	policy := s.getPolicy()
	if !CheckEgressAllowed(policy, qname) {
		s.metrics.IncRequest("dns", "deny")
		s.metrics.IncBlockedHost(qname)
		return buildDNSErrorResponse(query, questionEnd, dnsRcodeRefused), nil
	}

	s.metrics.IncRequest("dns", "allow")
	resp, err := s.forwardUpstream(query, useTCP)
	if err != nil {
		return buildDNSErrorResponse(query, questionEnd, dnsRcodeServFail), nil
	}
	return resp, nil
}

func (s *DNSServer) forwardUpstream(query []byte, useTCP bool) ([]byte, error) {
	if useTCP {
		conn, err := net.DialTimeout("tcp", s.upstreamAddr, s.timeout)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(s.timeout))

		var lenBuf [2]byte
		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(query)))
		if _, err := conn.Write(lenBuf[:]); err != nil {
			return nil, err
		}
		if _, err := conn.Write(query); err != nil {
			return nil, err
		}

		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			return nil, err
		}
		respLen := int(binary.BigEndian.Uint16(lenBuf[:]))
		resp := make([]byte, respLen)
		if _, err := io.ReadFull(conn, resp); err != nil {
			return nil, err
		}
		return resp, nil
	}

	conn, err := net.DialTimeout("udp", s.upstreamAddr, s.timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(s.timeout))

	if _, err := conn.Write(query); err != nil {
		return nil, err
	}

	buf := make([]byte, 4096)
	n, err := bufio.NewReader(conn).Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// parseDNSQName parses the first question's QNAME and returns the normalized domain name
// along with the byte offset immediately after the Question section (QNAME + QTYPE + QCLASS).
func parseDNSQName(msg []byte, offset int) (string, int, error) {
	var labels []string
	for {
		if offset >= len(msg) {
			return "", 0, errors.New("truncated QNAME")
		}
		l := int(msg[offset])
		offset++
		if l == 0 {
			break
		}
		if l >= 0xC0 || l > 63 {
			return "", 0, errors.New("invalid or compressed QNAME label")
		}
		if offset+l > len(msg) {
			return "", 0, errors.New("truncated QNAME label")
		}
		labels = append(labels, string(msg[offset:offset+l]))
		offset += l
	}

	if offset+4 > len(msg) {
		return "", 0, errors.New("truncated question footer")
	}
	// Skip QTYPE (2) and QCLASS (2).
	questionEnd := offset + 4
	qname := normalizeHost(strings.Join(labels, "."))
	if qname == "" {
		return "", 0, errors.New("empty QNAME")
	}
	return qname, questionEnd, nil
}

func buildDNSErrorResponse(query []byte, questionEnd int, rcode uint16) []byte {
	if questionEnd < dnsHeaderLen || questionEnd > len(query) {
		questionEnd = dnsHeaderLen
	}
	resp := make([]byte, questionEnd)
	// Transaction ID
	copy(resp[0:2], query[0:2])
	// Preserve Opcode and RD, set QR=1 (0x8000), RA=1 (0x0080), and RCODE.
	reqFlags := binary.BigEndian.Uint16(query[2:4])
	respFlags := uint16(0x8000) | (reqFlags & 0x7900) | 0x0080 | (rcode & 0x000F)
	binary.BigEndian.PutUint16(resp[2:4], respFlags)

	if questionEnd > dnsHeaderLen {
		binary.BigEndian.PutUint16(resp[4:6], 1) // QDCOUNT = 1
		copy(resp[dnsHeaderLen:questionEnd], query[dnsHeaderLen:questionEnd])
	} else {
		binary.BigEndian.PutUint16(resp[4:6], 0)
	}
	// ANCOUNT, NSCOUNT, ARCOUNT = 0
	return resp
}
