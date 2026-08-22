# Debian Trixie Chromium remote browser

A small Debian-based headed Chromium runtime with:

- Debian-packaged Chromium, Xvfb, Openbox, x11vnc, noVNC/websockify, Supervisor, and dumb-init;
- persistent Chromium profile under `/home/chromium/profile`;
- Chrome DevTools Protocol on port `9222`;
- native VNC on port `5900`;
- noVNC on port `6080`.

## Prebuilt image from GHCR

The image is built and published to GHCR automatically on every push to `main`, and rebuilt once per day so it picks up updated `debian:trixie-slim` base packages. Tags: `trixie` (current build) and `latest`.

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

To keep up with daily rebuilds, pull again periodically and recreate the container.

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

Open `http://127.0.0.1:5800/vnc.html` through an SSH tunnel or an authenticated reverse proxy.

Test CDP:

```bash
curl http://127.0.0.1:9222/json/version
```

If Hermes or whatever agent you use runs in another container on the same Podman network, do not publish CDP to a public interface; attach both containers to a private network and use `http://remote-chromium:9222`.

## Configuration

- `START_URL` — initial URL, default `about:blank`.
- `VNC_PASSWORD` — required VNC password. The container refuses to start if it is empty.
- `SCREEN_WIDTH`, `SCREEN_HEIGHT`, `SCREEN_DEPTH` — virtual display dimensions.
- `CDP_PORT`, `VNC_PORT`, `NOVNC_PORT` — internal service ports.

The image intentionally runs Chromium and the desktop services as the non-root `chromium` user. The profile bind mount must be writable by the container process. On SELinux systems, retain the `:Z` volume label.
