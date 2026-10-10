// Package e2e runs client and relay against each other over net.Pipe, with
// real loopback TCP/UDP servers as destinations.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/matin22766/witub/core/client"
	"github.com/matin22766/witub/core/proto"
	"github.com/matin22766/witub/core/relay"
)

const token = "s3cret"

type stubResolver struct{ calls atomic.Int32 }

func (r *stubResolver) Query(_ context.Context, q []byte) ([]byte, error) {
	r.calls.Add(1)
	resp := append([]byte(nil), q...)
	resp[2] |= 0x80
	return append(resp, 0xAA), nil // marker so the test can tell it came from here
}

func setup(t *testing.T) (*client.Client, *relay.Server, *stubResolver) {
	t.Helper()
	res := &stubResolver{}
	srv := &relay.Server{Token: token, Resolver: res, UDPIdle: time.Second, Logf: t.Logf}
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.ServeConn(ctx, b)
	c, err := client.Connect(ctx, a, token)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c, srv, res
}

func tcpEcho(t *testing.T) netip.AddrPort {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				io.Copy(c, c)
				c.(*net.TCPConn).CloseWrite()
				c.Close()
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).AddrPort()
}

func udpEcho(t *testing.T) netip.AddrPort {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			pc.WriteTo(buf[:n], from)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).AddrPort()
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestTCPEchoHalfClose(t *testing.T) {
	c, _, _ := setup(t)
	conn, err := c.DialTCP(ctx(t), proto.AddrFromAddrPort(tcpEcho(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	conn.Close() // half-close: the echo server sees EOF and closes after echoing
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestTCPLargeTransfer(t *testing.T) {
	c, srv, _ := setup(t)
	conn, err := c.DialTCP(ctx(t), proto.AddrFromAddrPort(tcpEcho(t)))
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8<<20)
	rand.Read(data)
	go func() {
		conn.Write(data)
		conn.Close()
	}()
	got, err := io.ReadAll(conn)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("mismatch: got %d bytes, err %v", len(got), err)
	}
	waitFor(t, func() bool {
		return srv.Stats.BytesUp.Load() == int64(len(data)) && srv.Stats.BytesDown.Load() == int64(len(data))
	})
}

func TestTCPDomainAddress(t *testing.T) {
	c, _, _ := setup(t)
	ap := tcpEcho(t)
	conn, err := c.DialTCP(ctx(t), proto.Addr{Domain: "localhost", Port: ap.Port()})
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("x"))
	conn.Close()
	if got, _ := io.ReadAll(conn); string(got) != "x" {
		t.Fatalf("got %q", got)
	}
}

func TestTCPRefused(t *testing.T) {
	c, _, _ := setup(t)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	ap := l.Addr().(*net.TCPAddr).AddrPort()
	l.Close()
	_, err := c.DialTCP(ctx(t), proto.AddrFromAddrPort(ap))
	if !errors.Is(err, proto.StatusRefused) {
		t.Fatalf("want refused, got %v", err)
	}
}

func TestUDPEcho(t *testing.T) {
	c, _, _ := setup(t)
	u, err := c.DialUDP(ctx(t), proto.AddrFromAddrPort(udpEcho(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	buf := make([]byte, proto.MaxDatagram)
	for _, msg := range []string{"one", "two", string(make([]byte, 1400))} {
		if err := u.WritePacket([]byte(msg)); err != nil {
			t.Fatal(err)
		}
		n, err := u.ReadPacket(buf)
		if err != nil || string(buf[:n]) != msg {
			t.Fatalf("got %q, %v", buf[:n], err)
		}
	}
}

func TestUDPIdleTimeout(t *testing.T) {
	c, srv, _ := setup(t)
	u, err := c.DialUDP(ctx(t), proto.AddrFromAddrPort(udpEcho(t)))
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	waitFor(t, func() bool { return srv.Stats.ActiveStreams.Load() == 1 })
	// UDPIdle is 1s in setup; with no traffic the relay should drop the flow.
	waitFor(t, func() bool { return srv.Stats.ActiveStreams.Load() == 0 })
}

// dnsQuery builds a minimal query for example.com with the given QTYPE.
func dnsQuery(qtype byte) []byte {
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	q = append(q, 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0)
	return append(q, 0, qtype, 0, 1)
}

func TestDNS(t *testing.T) {
	c, _, res := setup(t)
	resp, err := c.QueryDNS(ctx(t), dnsQuery(1))
	if err != nil || resp[len(resp)-1] != 0xAA || res.calls.Load() != 1 {
		t.Fatalf("A query: resp %x err %v calls %d", resp, err, res.calls.Load())
	}

	resp, err = c.QueryDNS(ctx(t), dnsQuery(28))
	if err != nil {
		t.Fatal(err)
	}
	if res.calls.Load() != 1 {
		t.Fatal("AAAA query reached the relay")
	}
	if resp[2]&0x80 == 0 || resp[3]&0x0f != 0 || resp[7] != 0 || len(resp) != len(dnsQuery(28)) {
		t.Fatalf("bad synthesized AAAA response %x", resp)
	}
}

func TestBadToken(t *testing.T) {
	srv := &relay.Server{Token: token, Logf: t.Logf}
	a, b := net.Pipe()
	go srv.ServeConn(context.Background(), b)
	_, err := client.Connect(ctx(t), a, "wrong")
	if !errors.Is(err, proto.StatusAuthFailed) {
		t.Fatalf("want auth failure, got %v", err)
	}
}

func TestSessionEndsOnServerShutdown(t *testing.T) {
	srv := &relay.Server{Token: token, Logf: t.Logf}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(context.Background())
	go srv.Serve(sctx, l)
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Connect(ctx(t), conn, token)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return srv.Stats.Sessions.Load() == 1 })
	cancel()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("client did not notice relay shutdown")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
