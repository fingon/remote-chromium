#!/bin/bash
# Integration test for the remote-chromium image.
#
# Starts an isolated container with a fresh temporary profile directory
# (created inside the repo: podman on macOS can only bind-mount paths
# shared into the machine VM, e.g. /Users), waits for the CDP endpoint
# to answer through the published port
# (host -> pasta -> socat -> chromium loopback), asserts the expected
# in-container listeners, then removes the container and temp profile.
#
# Environment overrides:
#   ENGINE            container engine          (default: podman)
#   IMAGE             image to test             (default: localhost/hermes-chromium:trixie)
#   TEST_CDP_PORTS    candidate host ports      (default: "19222 29222 39222")
#   WAIT_TIMEOUT_SEC  CDP readiness timeout     (default: 120)

set -euo pipefail

ENGINE="${ENGINE:-podman}"
IMAGE="${IMAGE:-localhost/hermes-chromium:trixie}"
read -ra CANDIDATE_PORTS <<<"${TEST_CDP_PORTS:-19222 29222 39222}"
WAIT_TIMEOUT_SEC="${WAIT_TIMEOUT_SEC:-120}"
CONTAINER_NAME="remote-chromium-itest"

log() { printf '[itest] %s\n' "$*"; }
die() {
    printf '[itest] FAIL: %s\n' "$*" >&2
    exit 1
}

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE_DIR="$(mktemp -d "$REPO_ROOT/.itest-profile.XXXXXX")"

cleanup() {
    set +e
    "$ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1
    rm -rf "$PROFILE_DIR" 2>/dev/null ||
        printf '[itest] WARN: could not remove %s; delete manually\n' "$PROFILE_DIR" >&2
}
trap cleanup EXIT

port_free() {
    ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

"$ENGINE" inspect "$IMAGE" >/dev/null 2>&1 ||
    die "image $IMAGE not found; run 'make build' first"

CDP_HOST_PORT=""
for p in "${CANDIDATE_PORTS[@]}"; do
    if port_free "$p"; then
        CDP_HOST_PORT="$p"
        break
    fi
done
[[ -n "$CDP_HOST_PORT" ]] || die "no free candidate host port among: ${CANDIDATE_PORTS[*]}"
log "using host port $CDP_HOST_PORT for CDP"

log "starting container from $IMAGE with temp profile $PROFILE_DIR"
"$ENGINE" run -d --replace --name "$CONTAINER_NAME" \
    --shm-size=512m \
    -e VNC_PASSWORD=itest-not-secret \
    -e START_URL=about:blank \
    -p "127.0.0.1:${CDP_HOST_PORT}:9222" \
    -v "$PROFILE_DIR:/home/chromium/profile:Z" \
    "$IMAGE" >/dev/null

container_running() {
    [[ "$("$ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" == "true" ]]
}

dump_logs_and_die() {
    "$ENGINE" logs --tail 60 "$CONTAINER_NAME" >&2 || true
    die "$1"
}

log "waiting up to ${WAIT_TIMEOUT_SEC}s for CDP on http://127.0.0.1:${CDP_HOST_PORT}/json/version"
deadline=$((SECONDS + WAIT_TIMEOUT_SEC))
version_json=""
while [[ -z "$version_json" ]]; do
    container_running || dump_logs_and_die "container exited before CDP became ready"
    ((SECONDS < deadline)) || dump_logs_and_die "timed out waiting for CDP"
    version_json="$(curl -fsS --max-time 2 "http://127.0.0.1:${CDP_HOST_PORT}/json/version" 2>/dev/null || true)"
    [[ -n "$version_json" ]] || sleep 1
done
log "CDP answered: $(printf '%s' "$version_json" | tr -d '\n')"
printf '%s' "$version_json" | grep -q '"Browser":' ||
    die "unexpected /json/version payload: $version_json"

log "asserting in-container listeners: chromium 127.0.0.1:9223, socat 0.0.0.0:9222"
listeners="$("$ENGINE" exec "$CONTAINER_NAME" cat /proc/net/tcp | awk '$4=="0A" {printf "%s ", $2}')"
has_listener() { [[ " $listeners " == *" $1 "* ]]; }
has_listener "0100007F:2407" || die "chromium not listening on 127.0.0.1:9223; listeners: $listeners"
has_listener "00000000:2406" || die "socat not listening on 0.0.0.0:9222; listeners: $listeners"

[[ -n "$(ls -A "$PROFILE_DIR" 2>/dev/null)" ]] ||
    die "temporary profile directory was never populated by chromium"

log "PASS: host->socat->chromium CDP chain verified; cleaning up"
