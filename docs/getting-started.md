# Getting started

From a clone to a device streaming on your phone, then running as a
service, then driven by an agent. Each step works before the next one
starts, so stop at the first that fails.

## What you need

- **A Mac.** loupe builds anywhere Go does (`make dist` cross-compiles
  Linux and Windows binaries), but iOS simulators, the launchd service and
  keeping the Mac awake are macOS only.
- **Go 1.26**, per `go.mod`. Nothing else is needed to build: the
  scrcpy-server jar and the Compose inspector agent are embedded, and the
  web client has no build step.
- **adb** for Android devices, and the Android emulator for AVDs. loupe
  finds the SDK through `ANDROID_HOME`, `ANDROID_SDK_ROOT`, then
  `~/Library/Android/sdk`.
- **Xcode with an iOS simulator runtime**, for simulators. loupe is
  developed against Xcode 27.
- **ffmpeg**, only for the `-synthetic` test pattern in step 1.
- **A TLS certificate the phone trusts**, to reach loupe from another
  device. A tailnet is the easy way; see step 3.

## Homebrew or a clone

```sh
brew install bmcreations/tap/loupe
brew install --HEAD bmcreations/tap/simcap   # only for iOS simulators
```

That installs `loupe` and, if you want it, simcap on your PATH, so every
`./loupe` below is `loupe`. `brew services start loupe` replaces step 4: it
runs `loupe -addr :18456` under launchd, in plain HTTP until
`config.json` names a certificate, and logs to
`$(brew --prefix)/var/log/loupe.log`. The steps below build from a clone,
which is what you want to change loupe itself or to sign simcap for a
physical iPhone.

## 1. Build it and check the client

```sh
git clone https://github.com/bmcreations/loupe
cd loupe
make build
./loupe -addr 127.0.0.1:8080 -synthetic -no-devices
```

Open the link the server prints; it carries the access token. You should
see a moving test pattern with a stats overlay at about 60 fps. That
exercises the browser's decoder and the transport with no device involved.
If it does not hold 60 fps, nothing on the device side will fix it.

## 2. Stream a real device

```sh
adb devices          # the device must be listed and authorized
./loupe -addr 127.0.0.1:8080
```

Phones are picked up live, so one plugged in later appears in the picker.
Every installed AVD is listed too, as `avd:<name>`, and opening one boots
it. loupe boots emulators with `-gpu host` by default, the only GPU mode
that reaches 60 fps. You don't need scrcpy installed, because loupe pushes
its own copy of the server jar on every launch.

For iOS simulators, build the helper once and restart. It needs Xcode:

```sh
make simcap          # clones bmcreations/simcap and builds it into .build/simcap
```

loupe uses `.build/simcap` when it runs from the checkout, and otherwise
the `simcap` on PATH, which is where the Homebrew formula puts it. Every
available simulator then appears as `sim:<udid>`, booted or not.
Physical iPhones are a preview; see [ios.md](ios.md).

## 3. Open it on your phone

Without TLS, loupe binds loopback only, so another device can't reach it.
It refuses any other address rather than send the token in the clear.

With Tailscale, turn on HTTPS certificates in the tailnet's admin console,
then issue a certificate for the Mac's tailnet name in the repo root and
serve on it:

```sh
tailscale cert my-mac.tailnet-0000.ts.net
./loupe -addr :18456 \
  -tls-cert my-mac.tailnet-0000.ts.net.crt \
  -tls-key my-mac.tailnet-0000.ts.net.key
```

Any certificate the phone trusts works the same way. loupe serves TLS
itself rather than behind a proxy, because the video goes over HTTP/3
(WebTransport), which needs UDP as well as TCP on the port. A proxy that
only carries TCP, such as `tailscale serve`, can't carry it.

In a terminal, the server prints a link and a QR code for the phone. Open
it once in Safari. The link sets a cookie and strips the token from the
address. To keep it, add it to the Home Screen: the installed app carries
its own credential and opens full screen.

## 4. Run it as a service

```sh
make agent-install   # write the LaunchAgent and start it
make agent-status    # state, pid, last exit
make agent-log       # tail loupe-run.log
```

The service runs `loupe-supervisor.sh` on port 18456 with TLS. Do step 3's
`tailscale cert` first: the supervisor uses the `*.ts.net.crt` it left in
the repo root, or the name in `LOUPE_TS_HOSTNAME`, and reissues the
certificate with `tailscale cert` when it is 30 days old. It restarts
the server after a crash and onto every new build, so from then on
`make build` is the deploy step. It also runs `-synthetic`, so the test
pattern is always there.

Under launchd the run log never holds the token. Read it from
`~/Library/Caches/loupe/token`, or fetch the phone link with
`GET /api/handoff`. While anyone is watching, loupe keeps the Mac from
idle-sleeping. Closing a MacBook's lid still sleeps it.

## 5. Connect an agent

```sh
cp loupe ~/.local/bin/loupe          # a copy, so make build can replace ./loupe
claude mcp add loupe -- ~/.local/bin/loupe mcp
```

The agent then has tools to list targets, read the UI tree, take
screenshots, tap, swipe, type, launch apps, read logs and crashes, and
install APKs. It claims a device before it drives one, and a person's input
on the same device holds the agent off for five seconds. Without MCP, the
same routes are plain HTTP:

```sh
curl -H "Authorization: Bearer $(cat ~/Library/Caches/loupe/token)" \
  "https://my-mac.tailnet-0000.ts.net:18456/api/ui?target=avd:Pixel_10&interactive=1"
```

[agent-api.md](agent-api.md) lists every route, and [mcp.md](mcp.md)
covers the MCP server.

## Next

- [running.md](running.md): every flag, the settings file, and how to read
  the stats overlay.
- [targets.md](targets.md): AVD lifecycle, the phone keyboard, the
  clipboard, device frames.
- [security.md](security.md): what the token protects, and pairing a
  device with its own revocable session.
- [traps.md](traps.md): search it for the symptom you see before debugging.
