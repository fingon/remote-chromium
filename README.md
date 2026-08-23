# Debian Trixie Chromium remote browser

A small Debian-based headed Chromium runtime with:

- Debian-packaged Chromium, Xvfb, Openbox, x11vnc, noVNC/websockify,
  Supervisor, and dumb-init;
- persistent Chromium profile under `/home/chromium/profile`;
- Chrome DevTools Protocol on port `9222`;
- native VNC on port `5900`;
- noVNC on port `6080`.

## Configuration

- `START_URL` — initial URL, default `about:blank`.
- `VNC_PASSWORD` — required VNC password. The container refuses
  to start if it is empty.
- `SCREEN_WIDTH`, `SCREEN_HEIGHT`, `SCREEN_DEPTH` — virtual display
  dimensions.
- `CDP_PORT` — externally published CDP port (socat listener).
- `CDP_INTERNAL_PORT` — Chromium's loopback CDP port.
- `VNC_PORT`, `NOVNC_PORT` — VNC service ports.

The image intentionally runs Chromium and the desktop services as the
non-root `chromium` user. The profile bind mount must be writable by the
container process. On SELinux systems, retain the `:Z` volume label.

## Prebuilt image from GHCR

The image is built and published to GHCR automatically on every push to
`main`, and rebuilt once per day so it picks up updated
`debian:trixie-slim` base packages. Tags: `trixie` (current build) and
`latest`.

```bash
podman pull ghcr.io/fingon/remote-chromium:trixie
```

Run it directly:

```bash
mkdir -p profile
export VNC_PASSWORD='use-a-real-password'
podman run -d \
  --name remote-chromium \
  --restart=unless-stopped \
  --userns=keep-id \
  --shm-size=2g \
  -e VNC_PASSWORD \
  -p 127.0.0.1:5800:6080 \
  -p 127.0.0.1:9222:9222 \
  -v "$PWD/profile:/home/chromium/profile:Z" \
  ghcr.io/fingon/remote-chromium:trixie
```

To keep up with daily rebuilds, pull again periodically and recreate the
container.

## Build locally with Podman

Direct Podman usage:

```bash
mkdir -p profile
podman build -t localhost/remote-chromium:trixie .
export VNC_PASSWORD='use-a-real-password'
podman run -d \
  --name remote-chromium \
  --restart=unless-stopped \
  --userns=keep-id \
  --shm-size=2g \
  -e VNC_PASSWORD \
  -p 127.0.0.1:5800:6080 \
  -p 127.0.0.1:9222:9222 \
  -v "$PWD/profile:/home/chromium/profile:Z" \
  localhost/remote-chromium:trixie
```

If the Podman Compose plugin is installed:

```bash
export VNC_PASSWORD='use-a-real-password'
podman compose up -d --build
```

Open `http://127.0.0.1:5800/vnc.html` through an SSH tunnel or an
authenticated reverse proxy.

Test CDP:

```bash
curl http://127.0.0.1:9222/json/version
```

If Hermes or whatever agent you use runs in another container on the same
Podman network, do not publish CDP to a public interface; attach both
containers to a private network and use `http://remote-chromium:9222`.

## Remote CDP access

Chromium binds its DevTools server to loopback only; since Chromium M113
the `--remote-debugging-address=0.0.0.0` flag is ignored. Inside the
container, `socat` therefore forwards `0.0.0.0:9222` to Chromium's
loopback port (9223), and the published host port stays bound to
`127.0.0.1`.

To reach CDP from another machine, tunnel over SSH:

```bash
ssh -N -L 9222:127.0.0.1:9222 user@docker-host
# then, from the other machine:
curl http://127.0.0.1:9222/json/version
```

Notes:

- Connect by IP address or `localhost`, never by hostname: the DevTools
  HTTP server closes connections whose `Host:` header is not an IP or
  localhost ("Host header is specified and is not an IP address or
  localhost").
- `--remote-allow-origins=*` is already set so browser-based WebSocket
  clients are accepted.
- An exposed CDP port grants full control of the browser (navigation,
  page content, downloads). Do not publish it beyond loopback without an
  authenticated proxy or VPN in front.

## Integration test

```bash
make test
```

Builds the image (if stale), starts an isolated container with a fresh
temporary profile directory on an automatically chosen host port, waits
for `http://127.0.0.1:<port>/json/version` to answer through the socat
chain, asserts the expected in-container listeners (chromium on loopback
9223, socat on `0.0.0.0:9222`), then removes the container and the
temporary profile.

Overrides: `ENGINE`, `IMAGE`, `TEST_CDP_PORTS`, `WAIT_TIMEOUT_SEC`.

## Linting

Hooks are managed with [prek](https://github.com/j178/prek); run
everything with `prek run --all-files`. All hook environments (including
Go-built `shfmt` and `hadolint`) are provisioned by prek itself, so no
host tooling is required. GitHub Actions run the same hooks on every push
and PR (`lint.yml`) and run the integration test before every GHCR
publish (`container.yml`).
