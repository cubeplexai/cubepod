package egress

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

var (
	// ErrInvalidClientHello is returned when the incoming stream is not a valid TLS ClientHello.
	ErrInvalidClientHello = errors.New("invalid TLS ClientHello")
	// ErrMissingSNI is returned when a TLS ClientHello does not contain a DNS server_name extension.
	ErrMissingSNI = errors.New("missing TLS SNI server_name extension")
)

const (
	tlsRecordTypeHandshake = 0x16
	tlsHandshakeClientHell = 0x01
	tlsExtServerName       = 0x0000
	tlsNameTypeHostName    = 0x00
	maxTLSRecordPayload    = 16384 + 2048
	sniPeekTimeout         = 5 * time.Second
)

// prefixConn wraps a net.Conn so that already-peeked bytes are read first before
// reading from the underlying connection.
type prefixConn struct {
	net.Conn
	prefix io.Reader
}

func newPrefixConn(conn net.Conn, peeked []byte) net.Conn {
	if len(peeked) == 0 {
		return conn
	}
	return &prefixConn{
		Conn:   conn,
		prefix: bytes.NewReader(peeked),
	}
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if c.prefix != nil {
		n, err := c.prefix.Read(p)
		if n > 0 {
			return n, nil
		}
		if err == io.EOF {
			c.prefix = nil
		} else if err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

// peekClientHelloSNI reads the initial TLS ClientHello record from conn, extracts the
// normalized SNI hostname, and returns a net.Conn that replays all peeked bytes.
func peekClientHelloSNI(conn net.Conn) (string, net.Conn, error) {
	_ = conn.SetReadDeadline(time.Now().Add(sniPeekTimeout))
	defer func() {
		_ = conn.SetReadDeadline(time.Time{})
	}()

	var header [5]byte
	n, err := io.ReadFull(conn, header[:])
	if err != nil {
		return "", newPrefixConn(conn, header[:n]), fmt.Errorf("%w: failed to read TLS record header: %v", ErrInvalidClientHello, err)
	}

	if header[0] != tlsRecordTypeHandshake || header[1] != 0x03 {
		return "", newPrefixConn(conn, header[:]), fmt.Errorf("%w: unexpected record type 0x%02x version 0x%02x%02x", ErrInvalidClientHello, header[0], header[1], header[2])
	}

	recordLen := int(binary.BigEndian.Uint16(header[3:5]))
	if recordLen < 4 || recordLen > maxTLSRecordPayload {
		return "", newPrefixConn(conn, header[:]), fmt.Errorf("%w: invalid record length %d", ErrInvalidClientHello, recordLen)
	}

	buf := make([]byte, 5+recordLen)
	copy(buf[:5], header[:])
	m, err := io.ReadFull(conn, buf[5:])
	replayed := newPrefixConn(conn, buf[:5+m])
	if err != nil {
		return "", replayed, fmt.Errorf("%w: failed to read TLS record payload: %v", ErrInvalidClientHello, err)
	}

	sni, err := parseClientHelloSNI(buf[5:])
	if err != nil {
		return "", replayed, err
	}
	return sni, replayed, nil
}

// parseClientHelloSNI parses a TLS Handshake ClientHello message and extracts the SNI hostname.
func parseClientHelloSNI(payload []byte) (string, error) {
	if len(payload) < 4 {
		return "", fmt.Errorf("%w: handshake message too short", ErrInvalidClientHello)
	}
	if payload[0] != tlsHandshakeClientHell {
		return "", fmt.Errorf("%w: expected ClientHello (0x01), got 0x%02x", ErrInvalidClientHello, payload[0])
	}

	hsLen := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if hsLen+4 > len(payload) {
		return "", fmt.Errorf("%w: truncated ClientHello (%d > %d)", ErrInvalidClientHello, hsLen+4, len(payload))
	}
	msg := payload[4 : 4+hsLen]

	// ClientHello:
	//   2 bytes client_version
	//  32 bytes random
	if len(msg) < 34 {
		return "", fmt.Errorf("%w: ClientHello missing version/random", ErrInvalidClientHello)
	}
	offset := 34

	// session_id (1-byte length + bytes)
	if offset >= len(msg) {
		return "", fmt.Errorf("%w: missing session_id length", ErrInvalidClientHello)
	}
	sessionIDLen := int(msg[offset])
	offset += 1 + sessionIDLen
	if offset+2 > len(msg) {
		return "", fmt.Errorf("%w: missing cipher_suites length", ErrInvalidClientHello)
	}

	// cipher_suites (2-byte length + bytes)
	cipherSuitesLen := int(binary.BigEndian.Uint16(msg[offset : offset+2]))
	offset += 2 + cipherSuitesLen
	if offset >= len(msg) {
		return "", fmt.Errorf("%w: missing compression_methods length", ErrInvalidClientHello)
	}

	// compression_methods (1-byte length + bytes)
	compLen := int(msg[offset])
	offset += 1 + compLen
	if offset == len(msg) {
		return "", ErrMissingSNI
	}
	if offset+2 > len(msg) {
		return "", fmt.Errorf("%w: missing extensions length", ErrInvalidClientHello)
	}

	// extensions (2-byte length + extension list)
	extTotalLen := int(binary.BigEndian.Uint16(msg[offset : offset+2]))
	offset += 2
	if offset+extTotalLen > len(msg) {
		return "", fmt.Errorf("%w: truncated extensions", ErrInvalidClientHello)
	}
	exts := msg[offset : offset+extTotalLen]

	for len(exts) >= 4 {
		extType := binary.BigEndian.Uint16(exts[0:2])
		extLen := int(binary.BigEndian.Uint16(exts[2:4]))
		exts = exts[4:]
		if extLen > len(exts) {
			return "", fmt.Errorf("%w: truncated extension 0x%04x", ErrInvalidClientHello, extType)
		}
		extData := exts[:extLen]
		exts = exts[extLen:]

		if extType == tlsExtServerName {
			if len(extData) < 2 {
				return "", ErrMissingSNI
			}
			listLen := int(binary.BigEndian.Uint16(extData[0:2]))
			if listLen+2 > len(extData) {
				return "", fmt.Errorf("%w: truncated server_name list", ErrInvalidClientHello)
			}
			list := extData[2 : 2+listLen]
			for len(list) >= 3 {
				nameType := list[0]
				nameLen := int(binary.BigEndian.Uint16(list[1:3]))
				list = list[3:]
				if nameLen > len(list) {
					return "", fmt.Errorf("%w: truncated server_name entry", ErrInvalidClientHello)
				}
				nameBytes := list[:nameLen]
				list = list[nameLen:]

				if nameType == tlsNameTypeHostName {
					sni := normalizeHost(string(nameBytes))
					if sni == "" {
						return "", ErrMissingSNI
					}
					return sni, nil
				}
			}
		}
	}

	return "", ErrMissingSNI
}
