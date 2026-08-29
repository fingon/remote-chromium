FROM golang:1.26-alpine AS cdp-proxy-build
WORKDIR /src
COPY cdp-proxy/go.mod cdp-proxy/go.sum ./
RUN go mod download
COPY cdp-proxy/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/cdp-proxy .

FROM debian:stable-slim

ENV DEBIAN_FRONTEND=noninteractive \
    DISPLAY=:99 \
    BROWSER_HOME=/home/chromium \
    START_URL=about:blank \
    SCREEN_WIDTH=1920 \
    SCREEN_HEIGHT=1080 \
    SCREEN_DEPTH=24 \
    CDP_PORT=9222 \
    CDP_INTERNAL_PORT=9223 \
    VNC_PORT=5900 \
    NOVNC_PORT=6080

COPY debian-packages.txt /tmp/

RUN apt-get update \
    && packages_file=/tmp/debian-packages.txt \
    && xargs -r apt-get install -y --no-install-recommends < "$packages_file" \
    && rm -rf /var/lib/apt/lists/* \
    && rm -f "$packages_file" \
    && useradd --create-home --home-dir /home/chromium --shell /bin/bash chromium \
    && mkdir -p /home/chromium/profile /home/chromium/.vnc /run/supervisor \
    && chown -R chromium:chromium /home/chromium /run/supervisor

COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh
COPY --chmod=0755 start-chromium.sh /usr/local/bin/start-chromium.sh
COPY --from=cdp-proxy-build --chmod=0755 /out/cdp-proxy /usr/local/bin/cdp-proxy
COPY supervisord.conf /etc/supervisor/supervisord.conf

USER chromium
WORKDIR /home/chromium

ARG CONTENT_KEY=""
LABEL org.opencontainers.image.content-key="${CONTENT_KEY}"

EXPOSE 5900 6080 9222

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/usr/local/bin/entrypoint.sh"]
