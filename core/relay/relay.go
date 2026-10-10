// Package relay is the phone side: it accepts one yamux session per USB
// connection and, for every stream, dials the requested destination using
// ordinary sockets. Because those sockets belong to the app, Android routes
// them through whatever VPN is active on the phone.
package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/matin22766/witub/core/proto"
)

const (
	handshakeTimeout = 10 * time.Second
	dialTimeout      = 10 * time.Second
	defaultUDPIdle   = 60 * time.Second
)

// Resolver answers a raw DNS query message with a raw DNS response message.
type Resolver interface {
	Query(ctx context.Context, msg []byte) ([]byte, error)
}

// UDPResolver forwards queries to a fixed upstream server.
type UDPResolver struct {
	Server string // host:port
}

func (r UDPResolver) Query(ctx context.Context, msg []byte) ([]byte, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", r.Server)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if _, err := c.Write(msg); err != nil {
		return nil, err
	}
	buf := make([]byte, proto.MaxDatagram)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

type Stats struct {
	BytesUp       atomic.Int64 // laptop -> internet
	BytesDown     atomic.Int64 // internet -> laptop
	ActiveStreams atomic.Int64
	Sessions      atomic.Int64
}

type Server struct {
	Token    string
	Resolver Resolver
	UDPIdle  time.Duration // zero means 60s
	Logf     func(format string, args ...any)
	Stats    Stats

	mu       sync.Mutex
	sessions map[*yamux.Session]struct{}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// Serve accepts connections on l until ctx is cancelled or l fails.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	stop := context.AfterFunc(ctx, func() {
		l.Close()
		s.closeSessions()
	})
	defer stop()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go func() {
			if err := s.ServeConn(ctx, c); err != nil && !errors.Is(err, io.EOF) {
				s.logf("relay: session from %s ended: %v", c.RemoteAddr(), err)
			}
		}()
	}
}

// ServeConn authenticates c and serves streams until the session closes.
func (s *Server) ServeConn(ctx context.Context, c net.Conn) error {
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	tok, err := proto.ReadHandshake(c)
	if err != nil {
		c.Close()
		return err
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
		proto.WriteStatus(c, proto.StatusAuthFailed)
		c.Close()
		return proto.StatusAuthFailed
	}
	if err := proto.WriteStatus(c, proto.StatusOK); err != nil {
		c.Close()
		return err
	}
	c.SetDeadline(time.Time{})

	sess, err := yamux.Server(c, proto.YamuxConfig())
	if err != nil {
		c.Close()
		return err
	}
	s.track(sess, true)
	defer s.track(sess, false)
	s.Stats.Sessions.Add(1)
	defer s.Stats.Sessions.Add(-1)

	for {
		st, err := sess.AcceptStreamWithContext(ctx)
		if err != nil {
			sess.Close()
			if sess.IsClosed() && !errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
		go s.handleStream(ctx, st)
	}
}

func (s *Server) track(sess *yamux.Session, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		if s.sessions == nil {
			s.sessions = make(map[*yamux.Session]struct{})
		}
		s.sessions[sess] = struct{}{}
	} else {
		delete(s.sessions, sess)
	}
}

func (s *Server) closeSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sess := range s.sessions {
		sess.Close()
	}
}

func (s *Server) handleStream(ctx context.Context, st *yamux.Stream) {
	defer st.Close()
	s.Stats.ActiveStreams.Add(1)
	defer s.Stats.ActiveStreams.Add(-1)

	st.SetReadDeadline(time.Now().Add(handshakeTimeout))
	req, err := proto.ReadOpen(st)
	if err != nil {
		proto.WriteStatus(st, proto.StatusBadRequest)
		return
	}
	st.SetReadDeadline(time.Time{})

	switch req.Kind {
	case proto.KindTCP:
		s.handleTCP(ctx, st, req.Addr)
	case proto.KindUDP:
		s.handleUDP(ctx, st, req.Addr)
	case proto.KindDNS:
		s.handleDNS(ctx, st)
	}
}

func (s *Server) handleTCP(ctx context.Context, st *yamux.Stream, dst proto.Addr) {
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "tcp", dst.String())
	if err != nil {
		proto.WriteStatus(st, dialStatus(err))
		return
	}
	defer c.Close()
	if err := proto.WriteStatus(st, proto.StatusOK); err != nil {
		return
	}
	pipe(st, c, &s.Stats.BytesUp, &s.Stats.BytesDown)
}

func (s *Server) handleUDP(ctx context.Context, st *yamux.Stream, dst proto.Addr) {
	d := net.Dialer{Timeout: dialTimeout}
	c, err := d.DialContext(ctx, "udp", dst.String())
	if err != nil {
		proto.WriteStatus(st, dialStatus(err))
		return
	}
	defer c.Close()
	if err := proto.WriteStatus(st, proto.StatusOK); err != nil {
		return
	}

	idle := s.UDPIdle
	if idle == 0 {
		idle = defaultUDPIdle
	}
	var last atomic.Int64
	touch := func() { last.Store(time.Now().UnixNano()) }
	touch()

	go func() {
		defer c.Close()
		buf := make([]byte, proto.MaxDatagram)
		for {
			n, err := proto.ReadDatagram(st, buf)
			if err != nil {
				return
			}
			touch()
			if _, err := c.Write(buf[:n]); err == nil {
				s.Stats.BytesUp.Add(int64(n))
			}
		}
	}()

	buf := make([]byte, proto.MaxDatagram)
	for {
		c.SetReadDeadline(time.Now().Add(idle))
		n, err := c.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() &&
				time.Since(time.Unix(0, last.Load())) < idle {
				continue // the laptop is still sending; keep the flow open
			}
			if errors.Is(err, syscall.ECONNREFUSED) {
				continue // ICMP port unreachable for one packet; not fatal
			}
			return
		}
		touch()
		if err := proto.WriteDatagram(st, buf[:n]); err != nil {
			return
		}
		s.Stats.BytesDown.Add(int64(n))
	}
}

func (s *Server) handleDNS(ctx context.Context, st *yamux.Stream) {
	if s.Resolver == nil {
		proto.WriteStatus(st, proto.StatusRefused)
		return
	}
	if err := proto.WriteStatus(st, proto.StatusOK); err != nil {
		return
	}
	buf := make([]byte, proto.MaxDatagram)
	for {
		n, err := proto.ReadDatagram(st, buf)
		if err != nil {
			return
		}
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := s.Resolver.Query(qctx, append([]byte(nil), buf[:n]...))
		cancel()
		if err != nil {
			s.logf("relay: dns query failed: %v", err)
			resp = servfail(buf[:n])
			if resp == nil {
				return
			}
		}
		if err := proto.WriteDatagram(st, resp); err != nil {
			return
		}
	}
}

// servfail turns a query into a SERVFAIL response so the laptop's resolver
// fails fast instead of waiting for its own timeout.
func servfail(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	r := append([]byte(nil), q...)
	r[2] |= 0x80               // QR
	r[3] = (r[3] & 0xf0) | 0x2 // RCODE=SERVFAIL
	r[3] |= 0x80               // RA
	return r
}

func dialStatus(err error) proto.Status {
	var ne net.Error
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return proto.StatusRefused
	case errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return proto.StatusTimeout
	}
	return proto.StatusUnreachable
}

type closeWriter interface{ CloseWrite() error }

// pipe copies in both directions, propagating half-closes, and returns when
// both directions are done. An error in either direction (as opposed to a
// clean EOF) tears down both sides so neither copy blocks forever.
func pipe(st *yamux.Stream, c net.Conn, up, down *atomic.Int64) {
	abort := func() {
		c.Close()
		st.Close()
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		n, err := io.Copy(c, st)
		up.Add(n)
		cw, ok := c.(closeWriter)
		if err != nil || !ok {
			abort()
			return
		}
		cw.CloseWrite()
	}()
	n, err := io.Copy(st, c)
	down.Add(n)
	if err != nil {
		abort()
	} else {
		st.Close() // yamux Close is a half-close (FIN); reads continue
	}
	wg.Wait()
}
