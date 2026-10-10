# Plan: "PdaNet-style" USB tether app (Android phone → Windows laptop) with VPN

## Context
Goal: share an Android phone's mobile internet with a Windows laptop over a USB cable, with traffic
protected by a VPN, the way PdaNet+ does. This is a **new standalone project (new repo)**, unrelated
to `tadog` (a Telegram bot), which stays untouched.

How PdaNet actually works (and why it answers the VPN question):
- It is **not** Android's built-in USB tethering. The phone app acts as a **socket relay**: the laptop
  sends "connect to X:443" requests over USB, and the *phone app itself* opens that socket.
- Because the sockets are opened by an ordinary Android app, **Android routes them through whatever
  VPN is active on the phone** (unless that VPN excludes our app via split tunnelling). So "use the
  phone's VPN" comes for free with this architecture: no server and no extra cost.
- A side effect: traffic looks like it comes from the phone, not a tethered device. Many carrier plans
  forbid hidden tethering. That is the user's call, but say so in the README.

**Decision:** build the socket-relay architecture. VPN v1 = "whatever VPN runs on the phone" ($0).
VPN v2 (optional) = built-in WireGuard client inside the relay. It needs a server you control or a
commercial VPN that gives out WireGuard configs: about $4–6/month for a small VPS (Hetzner, DigitalOcean,
Vultr), $0 on Oracle Cloud Always Free if you can get capacity, or about $5/month for a commercial
WireGuard-capable VPN.

## Architecture

```
 Windows laptop                                   USB (ADB)        Android phone
┌───────────────────────────────────────────┐                ┌──────────────────────────────┐
│ Apps → Wintun virtual adapter (10.7.0.2)  │                │ Foreground service           │
│   → gVisor userspace TCP/IP stack         │  adb forward   │  relay server (Go, gomobile) │
│   → per-flow yamux streams ───────────────┼── tcp:N ──────▶│  → net.Dial TCP/UDP (as app) │
│ DNS (10.7.0.1:53) → DNS stream            │                │  → DnsResolver (Kotlin)      │
│ Tray UI + adb manager                     │                │  → phone VPN / mobile data   │
└───────────────────────────────────────────┘                └──────────────────────────────┘
```

- **The TCP/IP stack is terminated on the laptop** (gVisor netstack). The phone only does "dial and
  pipe", which keeps the phone side small, light on battery, and free of root or VpnService.
- **Multiplexing:** `hashicorp/yamux` over a single adb-forwarded TCP connection. It gives one stream
  per TCP flow with built-in flow control, so there is no custom windowing.
- **UDP:** one yamux stream per UDP 5-tuple, carrying length-prefixed datagrams, with an idle timeout
  (60s). This covers QUIC, games and voice.
- **DNS:** the laptop adapter's DNS is 10.7.0.1. Queries go over a dedicated stream type, and the phone
  answers through Kotlin `DnsResolver.rawQuery` (API 29+) so the phone's VPN DNS and Private DNS are
  respected. v1 is IPv4-only, so AAAA records are stripped to avoid IPv6 blackholes.
- **Auth:** the relay listens on phone `127.0.0.1` only. Other phone apps could still reach it, so
  the desktop generates a random token and passes it with
  `adb shell am start-foreground-service ... --es token <t>`. The first frame on the session must
  present it.
- **Shared Go core** is compiled into both sides: the Windows exe, and the Android `.aar` via
  gomobile. Both ends then run the same protocol code.

## Repo layout
```
core/                 Go module (shared)
  proto/              stream-open header: {type TCP|UDP|DNS, dst ip, port}, token handshake
  relay/              phone-side server: accept yamux, dial, pipe, UDP sessions
  client/             laptop-side: Dialer interface used by the netstack handlers
  mobile/             gomobile bindings: Start(port, token, dnsCallback) / Stop() / Stats()
desktop/windows/      Go: wintun adapter, gVisor netstack, routes/DNS (netsh / winipcfg),
                      adb manager (detect device, forward, launch app), systray UI
android/              Kotlin app: TetherService (foreground, type connectedDevice),
                      start/stop UI, stats, battery-optimization prompt, links core.aar
installer/            Inno Setup script: exe + wintun.dll + bundled adb platform-tools
docs/protocol.md
```

Key libraries: `golang.zx2c4.com/wintun` (wintun.dll is signed and redistributable, so no custom
driver signing), `gvisor.dev/gvisor/pkg/tcpip` (Apache-2.0), `hashicorp/yamux`,
`fyne.io/systray`, `golang.org/x/mobile`. Reference implementations to read are `tun2socks`
(xjasonlyu) and `gnirehtet`. Check tun2socks's license (GPL) before copying code. Building directly
on the gVisor packages keeps licensing clean.

## Milestones
1. **M0: core protocol.** Write `core/proto`, `core/relay` and `core/client`. Add unit tests over
   `net.Pipe`: TCP echo, UDP echo, DNS stub, bad token rejected, stream close/reset propagation.
2. **M1: desktop prototype, no phone needed.** A Windows CLI creates the Wintun adapter
   (10.7.0.2/30, MTU 1400, metric 1) and adds the default route pair `0.0.0.0/1` and `128.0.0.0/1`.
   It sets DNS 10.7.0.1 and wires gVisor TCP/UDP forwarders to `core/client`. The client talks to a
   desktop build of `core/relay` on another PC over LAN, which exercises the whole data path first.
   It needs an admin manifest.
3. **M2: Android relay.** Build core.aar with gomobile. `TetherService` starts the relay on
   127.0.0.1:N with a persistent notification, a wake lock and DnsResolver bridging. Test it manually
   with `adb forward tcp:N tcp:N` plus the M1 CLI.
4. **M3: one-click Windows flow.** Bundle adb and detect the device (`adb devices`, handling
   "unauthorized"). Push the token and start the service through adb, then set up the forward and
   bring the tunnel up. The tray shows connected/bytes/speed, and the client auto-reconnects on
   unplug/replug. On disconnect it tears down routes so the laptop never loses its other connections.
5. **M4: packaging.** Inno Setup installer, code-signed exe (otherwise SmartScreen warns), APK
   release build. Write user docs: enable USB debugging, and install the OEM USB driver if Windows
   does not show "ADB Interface".
6. **M5 (optional, later):**
   - **Built-in WireGuard:** the relay dials through `wireguard-go` + netstack (userspace, no
     VpnService) using an imported `.conf`.
   - **No-USB-debugging mode:** Android Open Accessory (AOA) transport. It needs a WinUSB driver
     binding on Windows, so it is harder.
   - **Wi-Fi mode, IPv6, Windows service:** run as a Windows service plus an unprivileged tray app
     over a named pipe, which removes the UAC prompt.

## Risks / notes
- ADB throughput is roughly 20–40 MB/s on USB 2, which is enough for LTE and most 5G.
- Windows DNS leak through other adapters ("smart multi-homed resolution"). Mitigate with interface
  metric 1, and optionally NRPT or a firewall rule in a "strict" mode.
- Android 14+ requires a foreground-service type: use `connectedDevice`. Some OEMs kill background
  services aggressively, so prompt for the battery-optimization exemption.
- If the phone VPN uses split tunnelling or excludes our app, laptop traffic bypasses the VPN. Show
  "VPN active on phone: yes/no" in the UI (Android `ConnectivityManager` `TRANSPORT_VPN`).

## Verification
- `go test ./core/...` (pipe-based integration tests) on every milestone.
- End-to-end on a real phone and Windows laptop with Wi-Fi turned off:
  - Browse, run speedtest.net, and download a large file. Check that YouTube over QUIC (UDP) works.
  - With a VPN active on the phone, `curl ifconfig.me` on the laptop should return the **VPN exit IP**,
    which proves the "phone VPN covers laptop" claim. Without the VPN it should return the carrier IP.
  - Run dnsleaktest.com from the laptop: only the VPN/phone resolvers should appear.
  - Unplug and replug the cable: it auto-reconnects and the laptop's routes are restored when it
    disconnects.
  - Run for a 1-hour soak with the phone screen off. The service should survive, and `adb logcat`
    should show no crashes.
