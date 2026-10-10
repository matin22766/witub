// Package mobile is the gomobile surface used by the Android app:
//
//	gomobile bind -target=android -androidapi 26 -o core.aar ./mobile
//
// Only gomobile-compatible types (string, int64, []byte, interfaces) appear in
// exported signatures.
package mobile

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/matin22766/witub/core/relay"
)

// DNSResolver is implemented in Kotlin with android.net.DnsResolver.rawQuery
// so that queries honour the phone's VPN DNS and Private DNS settings.
type DNSResolver interface {
	Query(msg []byte) ([]byte, error)
}

// Logger receives relay log lines (forwarded to logcat by the app).
type Logger interface {
	Log(msg string)
}

type dnsAdapter struct{ r DNSResolver }

func (d dnsAdapter) Query(ctx context.Context, msg []byte) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		b, err := d.r.Query(msg)
		ch <- result{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

var (
	mu     sync.Mutex
	srv    *relay.Server
	cancel context.CancelFunc
)

// Start listens on addr (normally "127.0.0.1:<port>", reached from the laptop
// through `adb forward`) and serves until Stop is called. A nil resolver falls
// back to fallbackDNS (host:port) over UDP.
func Start(addr, token string, resolver DNSResolver, fallbackDNS string, logger Logger) error {
	mu.Lock()
	defer mu.Unlock()
	if srv != nil {
		return errors.New("relay already running")
	}
	if token == "" {
		return errors.New("empty token")
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s := &relay.Server{Token: token}
	if resolver != nil {
		s.Resolver = dnsAdapter{resolver}
	} else if fallbackDNS != "" {
		s.Resolver = relay.UDPResolver{Server: fallbackDNS}
	}
	if logger != nil {
		s.Logf = func(f string, a ...any) { logger.Log(fmt.Sprintf(f, a...)) }
	}
	ctx, c := context.WithCancel(context.Background())
	srv, cancel = s, c
	go s.Serve(ctx, l)
	return nil
}

func Stop() {
	mu.Lock()
	defer mu.Unlock()
	if cancel != nil {
		cancel()
	}
	srv, cancel = nil, nil
}

func IsRunning() bool {
	mu.Lock()
	defer mu.Unlock()
	return srv != nil
}

func BytesUp() int64   { return stat(func(s *relay.Server) int64 { return s.Stats.BytesUp.Load() }) }
func BytesDown() int64 { return stat(func(s *relay.Server) int64 { return s.Stats.BytesDown.Load() }) }
func ActiveStreams() int64 {
	return stat(func(s *relay.Server) int64 { return s.Stats.ActiveStreams.Load() })
}

// Connected reports whether a laptop session is currently attached.
func Connected() bool {
	return stat(func(s *relay.Server) int64 { return s.Stats.Sessions.Load() }) > 0
}

func stat(f func(*relay.Server) int64) int64 {
	mu.Lock()
	defer mu.Unlock()
	if srv == nil {
		return 0
	}
	return f(srv)
}
