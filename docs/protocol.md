# Wire protocol (v1)

Transport: one TCP connection from the laptop to the phone, normally
`adb forward tcp:7071 tcp:7071` to the relay on the phone's `127.0.0.1:7071`.

## Session handshake

```
laptop -> phone : "UTT1" | tokenLen u8 | token
phone  -> laptop: status u8          (0 = OK, 5 = auth failed, then close)
```

The token is random per connection. The desktop generates it and passes it to
the app with `adb shell am start-foreground-service ... --es token <t>`. It
stops other apps on the phone from using the relay's loopback port.

After the OK byte both sides run **yamux** (laptop = client, phone = server)
with a 4 MiB stream window and 15 s keepalive.

## Streams

Each yamux stream starts with an open request from the laptop:

```
kind u8 | atyp u8 | addr | port u16be      (atyp/addr/port absent for DNS)
kind: 1 TCP, 2 UDP, 3 DNS
atyp: 1 IPv4 (4 bytes), 4 IPv6 (16 bytes), 3 domain (len u8 | name)
```

The phone answers with one status byte:

| code | meaning |
|---|---|
| 0 | OK |
| 1 | bad request |
| 2 | connection refused |
| 3 | host unreachable |
| 4 | timeout |
| 5 | auth failed (handshake only) |

What follows depends on the kind:

- **TCP**: raw bytes in both directions. Closing a stream is a half-close (FIN)
  and is propagated to the remote socket with `CloseWrite`.
- **UDP**: frames of `len u16be | payload` in both directions. The phone closes
  the stream after 60 s with no traffic either way.
- **DNS**: the laptop sends framed queries and the phone answers each with a
  framed response (SERVFAIL if its resolver fails). On Android the resolver is
  `DnsResolver.rawQuery`, so the phone's VPN DNS and Private DNS apply. The
  laptop answers AAAA queries locally with an empty response, because v1 is
  IPv4-only.
