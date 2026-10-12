import base64
import contextlib
import http.client
import json
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


TIMEOUT_SEC = float(sys.argv[1])
POLL_INTERVAL_SEC = 0.2
SOCKET_TIMEOUT_SEC = 3
RECOVERY_TIMEOUT_SEC = 30
PROXY_URL = f"http://127.0.0.1:{os.environ['CDP_PORT']}"
PROFILE_SENTINEL = Path(os.environ["BROWSER_HOME"]) / "profile" / "idle-test-sentinel"
SENTINEL_CONTENT = "profile survives idle restart"


def browser_pid():
    output = subprocess.check_output(
        ["supervisorctl", "-c", "/etc/supervisor/supervisord.conf", "pid", "chromium"],
        text=True,
        timeout=SOCKET_TIMEOUT_SEC,
    )
    return int(output.strip())


def discovery():
    with urllib.request.urlopen(
        PROXY_URL + "/json/version", timeout=SOCKET_TIMEOUT_SEC
    ) as response:
        return json.load(response)


@contextlib.contextmanager
def connect():
    endpoint = urllib.parse.urlsplit(discovery()["webSocketDebuggerUrl"])
    with socket.create_connection(
        (endpoint.hostname, endpoint.port), timeout=SOCKET_TIMEOUT_SEC
    ) as conn:
        key = base64.b64encode(os.urandom(16)).decode()
        request = (
            f"GET {endpoint.path} HTTP/1.1\r\n"
            f"Host: {endpoint.netloc}\r\n"
            "Connection: Upgrade\r\n"
            "Upgrade: websocket\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n\r\n"
        )
        conn.sendall(request.encode())
        with conn.makefile("rb") as reader:
            status = reader.readline()
            if b" 101 " not in status:
                raise AssertionError(f"CDP upgrade failed: {status!r}")
            while True:
                line = reader.readline()
                if not line:
                    raise AssertionError("CDP connection closed during handshake")
                if line == b"\r\n":
                    break
            yield conn, reader


def read_exact(reader, size_bytes):
    data = reader.read(size_bytes)
    if len(data) != size_bytes:
        raise AssertionError("CDP connection closed unexpectedly")
    return data


def command(conn, reader, command_id):
    payload = json.dumps(
        {"id": command_id, "method": "Browser.getVersion"}, separators=(",", ":")
    ).encode()
    assert len(payload) < 126
    mask = os.urandom(4)
    masked = bytes(value ^ mask[index % len(mask)] for index, value in enumerate(payload))
    conn.sendall(bytes([0x81, 0x80 | len(payload)]) + mask + masked)
    while True:
        header = read_exact(reader, 2)
        size_bytes = header[1] & 0x7F
        if size_bytes == 126:
            size_bytes = struct.unpack("!H", read_exact(reader, 2))[0]
        elif size_bytes == 127:
            size_bytes = struct.unpack("!Q", read_exact(reader, 8))[0]
        assert header[1] & 0x80 == 0, "server frame must be unmasked"
        response = json.loads(read_exact(reader, size_bytes))
        if response.get("id") == command_id:
            assert "result" in response, response
            return


def wait_for_restart(previous_pid):
    deadline = time.monotonic() + TIMEOUT_SEC + RECOVERY_TIMEOUT_SEC
    while time.monotonic() < deadline:
        pid = browser_pid()
        if pid and pid != previous_pid:
            print(f"[itest] Chromium restarted: pid={previous_pid} -> pid={pid}", flush=True)
            wait_for_recovery()
            assert PROFILE_SENTINEL.read_text() == SENTINEL_CONTENT
            return pid
        time.sleep(POLL_INTERVAL_SEC)
    raise AssertionError("Chromium did not restart after CDP inactivity")


def wait_for_recovery():
    deadline = time.monotonic() + RECOVERY_TIMEOUT_SEC
    while time.monotonic() < deadline:
        try:
            discovery()
            status = subprocess.check_output(
                ["supervisorctl", "-c", "/etc/supervisor/supervisord.conf", "status", "chromium"],
                text=True,
                timeout=SOCKET_TIMEOUT_SEC,
            )
            assert status.split()[1] == "RUNNING", status
            return
        except (OSError, urllib.error.URLError, http.client.HTTPException, AssertionError, subprocess.CalledProcessError) as error:
            print(f"[itest] waiting for CDP recovery: {error}", flush=True)
            time.sleep(POLL_INTERVAL_SEC)
    raise AssertionError("CDP did not recover after Chromium restart")


def verify_no_repeated_restart(pid):
    deadline = time.monotonic() + 2 * TIMEOUT_SEC
    while time.monotonic() < deadline:
        assert browser_pid() == pid, "idle browser restarted again without a new connection"
        discovery()
        time.sleep(POLL_INTERVAL_SEC)


def main():
    PROFILE_SENTINEL.write_text(SENTINEL_CONTENT)
    with connect() as (conn, reader):
        command(conn, reader, 1)
    pid = browser_pid()
    restarted_pid = wait_for_restart(pid)
    verify_no_repeated_restart(restarted_pid)
    with connect() as (conn, reader):
        active_pid = browser_pid()
        deadline = time.monotonic() + 1.5 * TIMEOUT_SEC
        command_id = 1
        while time.monotonic() < deadline:
            command(conn, reader, command_id)
            command_id += 1
            assert browser_pid() == active_pid, "active CDP session restarted"
            time.sleep(TIMEOUT_SEC / 4)
        wait_for_restart(active_pid)
    print("[itest] CDP inactivity, restart suppression, and profile preservation verified", flush=True)


main()
