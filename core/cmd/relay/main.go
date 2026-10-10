// Command relay runs the phone-side relay on a desktop machine. It lets the
// laptop client be developed and tested over a LAN before the Android app
// exists (milestone M1).
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"os"
	"os/signal"

	"github.com/matin22766/witub/core/relay"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:7071", "address to listen on")
	token := flag.String("token", "", "shared secret the client must present (required)")
	dns := flag.String("dns", "1.1.1.1:53", "upstream DNS server for DNS streams")
	flag.Parse()
	if *token == "" {
		log.Fatal("-token is required")
	}

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	s := &relay.Server{Token: *token, Resolver: relay.UDPResolver{Server: *dns}}
	log.Printf("relay listening on %s", l.Addr())
	if err := s.Serve(ctx, l); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
