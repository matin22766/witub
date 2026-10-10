# witub

Share an Android phone's mobile internet with a Windows laptop over USB, the
way PdaNet does. The phone app acts as a **socket relay**: the laptop asks for
"connect to X:443" over the cable, and the phone app opens that socket itself.
Because those are ordinary app sockets, **any VPN active on the phone also
covers the laptop's traffic**, with no extra server.

> Many carrier plans restrict tethering. Check your plan's terms before use.

## Status

| Milestone | State |
|---|---|
| M0 shared Go core (protocol, relay, client, tests) | done |
| M1 Windows client (Wintun + gVisor netstack) | todo |
| M2 Android app (foreground service + core.aar) | todo |
| M3 one-click flow (bundled adb, tray UI) | todo |
| M4 packaging | todo |
| M5 built-in WireGuard, AOA, Wi-Fi mode | later |

See [docs/plan.md](docs/plan.md) for the full design and
[docs/protocol.md](docs/protocol.md) for the wire format.

## Layout

```
core/proto     wire format (handshake, stream open, datagram framing)
core/relay     phone side: accept yamux streams, dial, pipe
core/client    laptop side: DialTCP / DialUDP / QueryDNS over the session
core/mobile    gomobile bindings for the Android app
core/cmd/relay desktop build of the relay, for testing without a phone
core/e2e       client <-> relay integration tests
```

## Develop

```sh
cd core
go test -race ./...
go run ./cmd/relay -token dev            # stand-in for the phone
gomobile bind -target=android -androidapi 26 -o core.aar ./mobile
```
