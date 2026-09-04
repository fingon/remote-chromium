# Debian Stable Chromium remote browser

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
- `CDP_PORT` — externally published CDP port (cdp-proxy listener).
- `CDP_INTERNAL_PORT` — Chromium's loopback CDP port.
- `VNC_PORT`, `NOVNC_PORT` — VNC service ports.

The image intentionally runs Chromium and the desktop services as the
non-root `chromium` user. The profile bind mount must be writable by the
container process. On SELinux systems, retain the `:Z` volume label.
Because the container hostname changes on every recreate, stale Chromium
`Singleton*` profile locks are wiped on startup; otherwise a forcibly
restarted container would refuse to start ("profile appears to be in use
by another Chromium process ... on another computer").

### Rootless Podman bind-mount ownership

The `chromium` user has UID and GID 1000 inside the image. With rootless
Podman, use `--userns=keep-id:uid=1000,gid=1000` to map the user invoking
Podman to that account. Files Chromium creates in the profile bind mount then
remain owned by the invoking user on the host, regardless of the host user's
numeric UID and GID.

The same mapping applies to any other writable bind mount. For example, add
`-v "$PWD/workdir:/workdir:Z"` after creating `workdir` on the host. Do not
add the `:U` volume option: it recursively changes ownership of the host
directory instead of preserving the invoking user's ownership. A
world-writable directory is not required.

Confirm the numeric ownership from the host with:

```bash
ls -lnd profile
```

Run the same command for any other bind-mounted host directory, such as
`workdir`.

## Prebuilt image from GHCR

The image is built and published to GHCR automatically on every push to
`main`, and rebuilt once per day so it picks up updated
`debian:stable-slim` base content and newer versions of the installed Debian
packages. Tags: `stable` (current build) and `latest`.

```bash
podman pull ghcr.io/fingon/remote-chromium:stable
```

Run it directly:

```bash
mkdir -p profile
export VNC_PASSWORD='use-a-real-password'
podman run -d \
  --name remote-chromium \
  --restart=unless-stopped \
  --userns=keep-id:uid=1000,gid=1000 \
  --shm-size=2g \
  -e VNC_PASSWORD \
  -p 127.0.0.1:5800:6080 \
  -p 127.0.0.1:9222:9222 \
  -v "$PWD/profile:/home/chromium/profile:Z" \
  ghcr.io/fingon/remote-chromium:stable
```

To keep up with daily rebuilds, pull again periodically and recreate the
container.

## Build locally with Podman

Direct Podman usage:

```bash
mkdir -p profile
podman build -t localhost/remote-chromium:stable .
export VNC_PASSWORD='use-a-real-password'
podman run -d \
  --name remote-chromium \
  --restart=unless-stopped \
  --userns=keep-id:uid=1000,gid=1000 \
  --shm-size=2g \
  -e VNC_PASSWORD \
  -p 127.0.0.1:5800:6080 \
  -p 127.0.0.1:9222:9222 \
  -v "$PWD/profile:/home/chromium/profile:Z" \
  localhost/remote-chromium:stable
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
the `--remote-debugging-address=0.0.0.0` flag is ignored, and since M66
the DevTools HTTP endpoint also rejects requests whose `Host:` header is
not an IP address or localhost (anti-DNS-rebinding). Inside the
container, `cdp-proxy` therefore forwards `0.0.0.0:9222` to Chromium's
loopback port (9223) while rewriting the `Host:` header to the loopback
upstream, and points `webSocketDebuggerUrl` in discovery responses back
at the host the client used — so connecting by hostname works end to
end:

```bash
curl http://fw.lan:9222/json/version   # works; Host header rewritten in-container
```

The published host port stays bound to `127.0.0.1`.

To reach CDP from another machine, tunnel over SSH:

```bash
ssh -N -L 9222:127.0.0.1:9222 user@docker-host
# then, from the other machine:
curl http://127.0.0.1:9222/json/version
```

Notes:

- `--remote-allow-origins=*` is already set so browser-based WebSocket
  clients are accepted.
- Discovery bodies are rewritten textually: a page URL or title that
  literally contains the internal `127.0.0.1:<CDP_INTERNAL_PORT>` string
  would appear rewritten too.
- An exposed CDP port grants full control of the browser (navigation,
  page content, downloads). Do not publish it beyond loopback without an
  authenticated proxy or VPN in front.

## Integration test

```bash
make test
```

Builds the image (if stale), starts an isolated container with a fresh
temporary profile directory on an automatically chosen host port, waits
for `http://127.0.0.1:<port>/json/version` to answer through the
cdp-proxy chain, asserts the expected in-container listeners (chromium on
loopback 9223, cdp-proxy on `0.0.0.0:9222`), verifies hostname-style
`Host:` headers are accepted with URLs rewritten to the requested host,
then removes the container and the temporary profile.

Overrides: `ENGINE`, `IMAGE`, `TEST_CDP_PORTS`, `WAIT_TIMEOUT_SEC`.
Set `ITEST_DEBUG=1` for shell tracing; failures print a diagnostics
bundle, and CI uploads one as the `integration-test-diagnostics` artifact.

## Linting

Hooks are managed with [prek](https://github.com/j178/prek); run
everything with `prek run --all-files`. All hook environments (including
Go-built `shfmt` and `hadolint`) are provisioned by prek itself, so no
host tooling is required. GitHub Actions run the same hooks on every push
and PR (`lint.yml`) and run the integration test before every GHCR
publish (`container.yml`).
