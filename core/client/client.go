// Package client is the laptop side: it authenticates to the phone relay over
// one connection (an adb-forwarded TCP socket in production) and opens one
// yamux stream per TCP flow, UDP flow or DNS query.
package client

import (
	"context"
	"net"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/matin22766/witub/core/proto"
)

const defaultOpenTimeout = 15 * time.Second

type Client struct {
	// FilterAAAA answers AAAA queries locally with an empty response. The
	// tunnel is IPv4-only in v1, so handing out IPv6 addresses would only make
	// applications wait for a connect timeout before falling back.
	FilterAAAA bool

	sess *yamux.Session
}

// Connect performs the handshake on c and starts the yamux session. The
// client takes ownership of c.
func Connect(ctx context.Context, c net.Conn, token string) (*Client, error) {
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(defaultOpenTimeout)
	}
	c.SetDeadline(dl)
	if err := proto.WriteHandshake(c, token); err != nil {
		c.Close()
		return nil, err
	}
	if err := proto.ReadStatus(c); err != nil {
		c.Close()
		return nil, err
	}
	c.SetDeadline(time.Time{})
	sess, err := yamux.Client(c, proto.YamuxConfig())
	if err != nil {
		c.Close()
		return nil, err
	}
	return &Client{FilterAAAA: true, sess: sess}, nil
}

func (c *Client) Close() error { return c.sess.Close() }

// Done is closed when the session ends (cable unplugged, relay stopped, ...).
func (c *Client) Done() <-chan struct{} { return c.sess.CloseChan() }

func (c *Client) open(ctx context.Context, req proto.OpenRequest) (*yamux.Stream, error) {
	st, err := c.sess.OpenStream()
	if err != nil {
		return nil, err
	}
	dl, ok := ctx.Deadline()
	if !ok {
		dl = time.Now().Add(defaultOpenTimeout)
	}
	st.SetDeadline(dl)
	if err := proto.WriteOpen(st, req); err != nil {
		st.Close()
		return nil, err
	}
	if err := proto.ReadStatus(st); err != nil {
		st.Close()
		return nil, err
	}
	st.SetDeadline(time.Time{})
	return st, nil
}

// DialTCP asks the phone to connect to dst. The returned conn's Close sends a
// FIN (half-close), matching TCP semantics.
func (c *Client) DialTCP(ctx context.Context, dst proto.Addr) (net.Conn, error) {
	return c.open(ctx, proto.OpenRequest{Kind: proto.KindTCP, Addr: dst})
}

// UDPConn is one UDP flow to a fixed destination.
type UDPConn struct {
	st *yamux.Stream
}

func (c *Client) DialUDP(ctx context.Context, dst proto.Addr) (*UDPConn, error) {
	st, err := c.open(ctx, proto.OpenRequest{Kind: proto.KindUDP, Addr: dst})
	if err != nil {
		return nil, err
	}
	return &UDPConn{st: st}, nil
}

// ReadPacket reads one datagram; buf should hold proto.MaxDatagram bytes.
func (u *UDPConn) ReadPacket(buf []byte) (int, error) { return proto.ReadDatagram(u.st, buf) }
func (u *UDPConn) WritePacket(p []byte) error         { return proto.WriteDatagram(u.st, p) }
func (u *UDPConn) Close() error                       { return u.st.Close() }

// QueryDNS resolves a raw DNS query through the phone's resolver.
func (c *Client) QueryDNS(ctx context.Context, msg []byte) ([]byte, error) {
	if c.FilterAAAA {
		if resp, ok := emptyAAAAResponse(msg); ok {
			return resp, nil
		}
	}
	st, err := c.open(ctx, proto.OpenRequest{Kind: proto.KindDNS})
	if err != nil {
		return nil, err
	}
	defer st.Close()
	if dl, ok := ctx.Deadline(); ok {
		st.SetDeadline(dl)
	}
	if err := proto.WriteDatagram(st, msg); err != nil {
		return nil, err
	}
	buf := make([]byte, proto.MaxDatagram)
	n, err := proto.ReadDatagram(st, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
