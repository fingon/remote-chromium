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
# On any failure a diagnostics bundle is printed to stderr; when
# ITEST_DIAG_DIR points at an existing directory it is also written to
# $ITEST_DIAG_DIR/itest-diag.txt (CI uploads that as an artifact).
#
# Environment overrides:
#   ENGINE            container engine          (default: podman)
#   IMAGE             image to test             (default: localhost/hermes-chromium:trixie)
#   TEST_CDP_PORTS    candidate host ports      (default: "19222 29222 39222")
#   WAIT_TIMEOUT_SEC  CDP readiness timeout     (default: 120)
#   ITEST_DEBUG       1 enables shell tracing   (default: 0)
#   ITEST_DIAG_DIR    directory for diag bundle

set -euo pipefail
if [[ "${ITEST_DEBUG:-0}" == "1" ]]; then
    set -x
fi

ENGINE="${ENGINE:-podman}"
IMAGE="${IMAGE:-localhost/hermes-chromium:trixie}"
read -ra CANDIDATE_PORTS <<<"${TEST_CDP_PORTS:-19222 29222 39222}"
WAIT_TIMEOUT_SEC="${WAIT_TIMEOUT_SEC:-120}"
CONTAINER_NAME="remote-chromium-itest"
CDP_PORT="${CDP_PORT:-9222}"
CDP_INTERNAL_PORT="${CDP_INTERNAL_PORT:-9223}"
DIAG_DIR="${ITEST_DIAG_DIR:-}"
DUMPED=0
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE_DIR="$(mktemp -d "$REPO_ROOT/.itest-profile.XXXXXX")"
# Rootless podman maps container uid 1000 (chromium) into a subuid range,
# so a dir owned by the invoking user would be read-only inside the
# container (SingletonLock failure, chromium exits 21). The dir is a
# freshly created throwaway, so world-writable is fine here.
chmod 0777 "$PROFILE_DIR"

log() { printf '[itest] %s\n' "$*"; }

dump_diagnostics() {
    local reason="$1"
    DUMPED=1
    local target=/dev/stderr
    if [[ -n "$DIAG_DIR" ]]; then
        mkdir -p "$DIAG_DIR"
        target="$DIAG_DIR/itest-diag.txt"
    fi
    {
        echo "=== [itest] diagnostics: $reason ==="
        echo "--- versions ---"
        uname -a || true
        "$ENGINE" version 2>&1 | head -5 || true
        echo "--- container state ---"
        "$ENGINE" inspect -f 'status={{.State.Status}} exit={{.State.ExitCode}} error={{.State.Error}} oom={{.State.OOMKilled}}' \
            "$CONTAINER_NAME" 2>&1 || true
        echo "--- processes inside container ---"
        # single-quoted on purpose: expansion happens inside the container
        # shellcheck disable=SC2016
        "$ENGINE" exec "$CONTAINER_NAME" bash -c \
            'for d in /proc/[0-9]*; do printf "%s " "${d##*/}"; tr "\0" " " <"$d/cmdline" 2>/dev/null || true; echo; done' \
            2>&1 || true
        echo "--- listeners inside container (/proc/net/tcp tcp6) ---"
        "$ENGINE" exec "$CONTAINER_NAME" bash -c 'cat /proc/net/tcp /proc/net/tcp6' 2>&1 || true
        echo "--- endpoint probes ---"
        if container_probe "$CDP_INTERNAL_PORT"; then
            echo "in-container 127.0.0.1:$CDP_INTERNAL_PORT UP"
        else
            echo "in-container 127.0.0.1:$CDP_INTERNAL_PORT DOWN"
        fi
        if container_probe "$CDP_PORT"; then
            echo "in-container 127.0.0.1:$CDP_PORT UP"
        else
            echo "in-container 127.0.0.1:$CDP_PORT DOWN"
        fi
        echo "--- curl -v against published port ---"
        curl -sv --max-time 3 "http://127.0.0.1:${CDP_HOST_PORT}/json/version" 2>&1 | tail -25 || true
        echo "--- profile directory ---"
        ls -la "$PROFILE_DIR" 2>&1 || true
        echo "--- socat stderr (last 40) ---"
        "$ENGINE" exec "$CONTAINER_NAME" bash -c 'tail -40 /tmp/supervisor/socat-cdp.err 2>/dev/null' || true
        echo "--- container logs minus socat noise (last 400) ---"
        "$ENGINE" logs --tail 400 "$CONTAINER_NAME" 2>&1 |
            grep -vE 'socat\[[0-9]+\] [WE] ' || true
    } >"$target" 2>&1
    if [[ -n "$DIAG_DIR" ]]; then
        cat "$target" >&2
    fi
}

die() {
    if ((!DUMPED)); then
        dump_diagnostics "$1"
    fi
    printf '[itest] FAIL: %s\n' "$1" >&2
    exit 1
}

finish() {
    local rc=$?
    if ((rc != 0 && !DUMPED)); then
        dump_diagnostics "unexpected failure rc=${rc}"
    fi
    cleanup
}

cleanup() {
    set +e
    "$ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1
    rm -rf "$PROFILE_DIR" 2>/dev/null ||
        printf '[itest] WARN: could not remove %s; delete manually\n' "$PROFILE_DIR" >&2
}

trap finish EXIT

port_free() {
    ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}

container_running() {
    [[ "$("$ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" == "true" ]]
}

container_probe() {
    "$ENGINE" exec "$CONTAINER_NAME" bash -c "exec<>/dev/tcp/127.0.0.1/$1" 2>/dev/null
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

log "waiting up to ${WAIT_TIMEOUT_SEC}s for CDP on http://127.0.0.1:${CDP_HOST_PORT}/json/version"
deadline=$((SECONDS + WAIT_TIMEOUT_SEC))
version_json=""
chromium_seen=no
while [[ -z "$version_json" ]]; do
    container_running || die "container exited before CDP became ready"
    ((SECONDS < deadline)) || {
        if [[ "$chromium_seen" == "no" ]]; then
            die "timed out: chromium never opened CDP on 127.0.0.1:${CDP_INTERNAL_PORT} inside the container"
        else
            die "timed out: CDP was up inside the container but host->socat forwarding on ${CDP_HOST_PORT} stayed broken"
        fi
    }
    if container_probe "$CDP_INTERNAL_PORT"; then
        chromium_seen=yes
    fi
    version_json="$(curl -fsS --max-time 2 "http://127.0.0.1:${CDP_HOST_PORT}/json/version" 2>/dev/null || true)"
    [[ -n "$version_json" ]] || sleep 1
done
log "CDP answered: $(printf '%s' "$version_json" | tr -d '\n')"
printf '%s' "$version_json" | grep -q '"Browser":' ||
    die "unexpected /json/version payload: $version_json"

log "asserting in-container listeners: chromium 127.0.0.1:${CDP_INTERNAL_PORT}, socat 0.0.0.0:${CDP_PORT}"
listeners="$("$ENGINE" exec "$CONTAINER_NAME" cat /proc/net/tcp | awk '$4=="0A" {printf "%s ", $2}')"
has_listener() { [[ " $listeners " == *" $1 "* ]]; }
has_listener "0100007F:2407" || die "chromium not listening on 127.0.0.1:9223; listeners: $listeners"
has_listener "00000000:2406" || die "socat not listening on 0.0.0.0:9222; listeners: $listeners"

[[ -n "$(ls -A "$PROFILE_DIR" 2>/dev/null)" ]] ||
    die "temporary profile directory was never populated by chromium"

log "PASS: host->socat->chromium CDP chain verified; cleaning up"
