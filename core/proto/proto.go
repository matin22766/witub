// Package proto defines the wire format shared by the laptop client and the
// phone relay.
//
// Session setup (once per USB connection, before yamux starts):
//
//	client -> relay: "UTT1" | tokenLen(1) | token
//	relay  -> client: status(1)
//
// After a successful handshake the client side runs a yamux client and the
// relay side a yamux server. Every yamux stream begins with an OpenRequest
// written by the client and answered by a single status byte from the relay:
//
//	kind(1) | atyp(1) | addr | port(2)      (addr omitted for KindDNS)
//
// KindTCP streams then carry raw bytes; KindUDP and KindDNS streams carry
// length-prefixed datagrams (see WriteDatagram).
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"time"

	"github.com/hashicorp/yamux"
)

const (
	Magic       = "UTT1"
	MaxTokenLen = 255
	MaxDatagram = 65535
)

type Kind byte

const (
	KindTCP Kind = 1
	KindUDP Kind = 2
	KindDNS Kind = 3
)

const (
	atypIPv4   = 1
	atypDomain = 3
	atypIPv6   = 4
)

type Status byte

const (
	StatusOK          Status = 0
	StatusBadRequest  Status = 1
	StatusRefused     Status = 2
	StatusUnreachable Status = 3
	StatusTimeout     Status = 4
	StatusAuthFailed  Status = 5
)

func (s Status) Error() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusBadRequest:
		return "bad request"
	case StatusRefused:
		return "connection refused"
	case StatusUnreachable:
		return "host unreachable"
	case StatusTimeout:
		return "timeout"
	case StatusAuthFailed:
		return "authentication failed"
	}
	return "status " + strconv.Itoa(int(s))
}

// Addr is a destination: either an IP or a domain name, plus a port.
type Addr struct {
	IP     netip.Addr
	Domain string
	Port   uint16
}

func AddrFromAddrPort(ap netip.AddrPort) Addr {
	return Addr{IP: ap.Addr().Unmap(), Port: ap.Port()}
}

func (a Addr) String() string {
	host := a.Domain
	if a.IP.IsValid() {
		host = a.IP.String()
	}
	return netJoin(host, a.Port)
}

func netJoin(host string, port uint16) string {
	if ip, err := netip.ParseAddr(host); err == nil && ip.Is6() {
		return "[" + host + "]:" + strconv.Itoa(int(port))
	}
	return host + ":" + strconv.Itoa(int(port))
}

type OpenRequest struct {
	Kind Kind
	Addr Addr
}

func WriteHandshake(w io.Writer, token string) error {
	if len(token) > MaxTokenLen {
		return errors.New("proto: token too long")
	}
	buf := make([]byte, 0, len(Magic)+1+len(token))
	buf = append(buf, Magic...)
	buf = append(buf, byte(len(token)))
	buf = append(buf, token...)
	_, err := w.Write(buf)
	return err
}

func ReadHandshake(r io.Reader) (token string, err error) {
	var hdr [len(Magic) + 1]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", err
	}
	if string(hdr[:len(Magic)]) != Magic {
		return "", errors.New("proto: bad magic")
	}
	tok := make([]byte, hdr[len(Magic)])
	if _, err := io.ReadFull(r, tok); err != nil {
		return "", err
	}
	return string(tok), nil
}

func WriteOpen(w io.Writer, req OpenRequest) error {
	buf := []byte{byte(req.Kind)}
	if req.Kind != KindDNS {
		a := req.Addr
		switch {
		case a.IP.Is4():
			ip := a.IP.As4()
			buf = append(buf, atypIPv4)
			buf = append(buf, ip[:]...)
		case a.IP.Is6():
			ip := a.IP.As16()
			buf = append(buf, atypIPv6)
			buf = append(buf, ip[:]...)
		case a.Domain != "" && len(a.Domain) <= 255:
			buf = append(buf, atypDomain, byte(len(a.Domain)))
			buf = append(buf, a.Domain...)
		default:
			return fmt.Errorf("proto: invalid address %q", a.String())
		}
		buf = binary.BigEndian.AppendUint16(buf, a.Port)
	}
	_, err := w.Write(buf)
	return err
}

func ReadOpen(r io.Reader) (OpenRequest, error) {
	var req OpenRequest
	var b [2]byte
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return req, err
	}
	req.Kind = Kind(b[0])
	switch req.Kind {
	case KindDNS:
		return req, nil
	case KindTCP, KindUDP:
	default:
		return req, fmt.Errorf("proto: unknown kind %d", b[0])
	}
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return req, err
	}
	switch b[0] {
	case atypIPv4:
		var ip [4]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return req, err
		}
		req.Addr.IP = netip.AddrFrom4(ip)
	case atypIPv6:
		var ip [16]byte
		if _, err := io.ReadFull(r, ip[:]); err != nil {
			return req, err
		}
		req.Addr.IP = netip.AddrFrom16(ip)
	case atypDomain:
		if _, err := io.ReadFull(r, b[:1]); err != nil {
			return req, err
		}
		name := make([]byte, b[0])
		if _, err := io.ReadFull(r, name); err != nil {
			return req, err
		}
		req.Addr.Domain = string(name)
	default:
		return req, fmt.Errorf("proto: unknown address type %d", b[0])
	}
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return req, err
	}
	req.Addr.Port = binary.BigEndian.Uint16(b[:])
	return req, nil
}

func WriteStatus(w io.Writer, s Status) error {
	_, err := w.Write([]byte{byte(s)})
	return err
}

// ReadStatus returns nil for StatusOK and the Status as an error otherwise.
func ReadStatus(r io.Reader) error {
	var b [1]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return err
	}
	if s := Status(b[0]); s != StatusOK {
		return s
	}
	return nil
}

// WriteDatagram writes len(2) | payload in a single Write so concurrent
// writers on a stream never interleave a frame.
func WriteDatagram(w io.Writer, p []byte) error {
	if len(p) > MaxDatagram {
		return errors.New("proto: datagram too large")
	}
	buf := make([]byte, 2+len(p))
	binary.BigEndian.PutUint16(buf, uint16(len(p)))
	copy(buf[2:], p)
	_, err := w.Write(buf)
	return err
}

// ReadDatagram reads one frame into buf (which must hold MaxDatagram bytes)
// and returns the payload length.
func ReadDatagram(r io.Reader, buf []byte) (int, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, err
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n > len(buf) {
		return 0, io.ErrShortBuffer
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return 0, err
	}
	return n, nil
}

// YamuxConfig is the session configuration both ends must use. The stream
// window is raised from yamux's 256 KiB default so a single download can fill
// a USB 2 link.
func YamuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.MaxStreamWindowSize = 4 << 20
	c.KeepAliveInterval = 15 * time.Second
	c.LogOutput = io.Discard
	return c
}
