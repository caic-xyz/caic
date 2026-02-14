"""Tests for relay.py graceful shutdown via null-byte sentinel."""

import json
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time

RELAY_PY = os.path.join(os.path.dirname(__file__), "relay.py")


def _make_env(relay_dir):
    """Return env dict with CAIC_RELAY_DIR set."""
    env = os.environ.copy()
    env["CAIC_RELAY_DIR"] = relay_dir
    return env


def _wait_for_socket(sock_path, timeout=5):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if os.path.exists(sock_path):
            try:
                s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                s.connect(sock_path)
                s.close()
                return
            except OSError:
                pass
        time.sleep(0.05)
    raise TimeoutError("relay socket did not appear")


def _cleanup(relay_dir):
    """Kill any leftover relay daemon."""
    pid_path = os.path.join(relay_dir, "pid")
    if os.path.exists(pid_path):
        try:
            with open(pid_path) as f:
                pid = int(f.read().strip())
            os.kill(pid, 9)
        except (OSError, ValueError):
            pass
    shutil.rmtree(relay_dir, ignore_errors=True)


def test_close_stdin_sentinel():
    """Closing attach client's stdin sends null byte, relay closes proc.stdin,
    subprocess exits, relay daemon exits."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    out_path = os.path.join(relay_dir, "subprocess-out")
    env = _make_env(relay_dir)

    # Shell script: copy stdin to a file, write a marker on exit.
    script = os.path.join(relay_dir, "test.sh")
    with open(script, "w") as f:
        f.write(f"#!/bin/sh\ncat > {out_path}\necho DONE >> {out_path}\n")
    os.chmod(script, 0o755)

    try:
        proc = subprocess.Popen(
            [sys.executable, RELAY_PY, "serve-attach", "--dir", relay_dir, "--", "/bin/sh", script],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        # Send data.
        proc.stdin.write(b"hello\n")
        proc.stdin.flush()
        time.sleep(0.3)

        # Close stdin — triggers null-byte sentinel.
        proc.stdin.close()

        # Attach client should exit, then daemon should exit (subprocess gets EOF).
        proc.wait(timeout=10)

        # Verify subprocess received data and exited cleanly.
        with open(out_path) as f:
            content = f.read()
        assert "hello" in content, f"expected 'hello' in output, got: {content!r}"
        assert "DONE" in content, f"subprocess did not exit cleanly (no DONE marker), got: {content!r}"
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_ssh_drop_keeps_subprocess():
    """Killing the attach client abruptly (simulating SSH drop) does NOT close
    proc.stdin — the subprocess stays alive and is reconnectable."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    sock_path = os.path.join(relay_dir, "relay.sock")
    pid_path = os.path.join(relay_dir, "pid")
    env = _make_env(relay_dir)

    try:
        proc = subprocess.Popen(
            [sys.executable, RELAY_PY, "serve-attach", "--dir", relay_dir, "--", "cat"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        # Send data.
        proc.stdin.write(b"test\n")
        proc.stdin.flush()
        time.sleep(0.3)

        # Kill the attach client abruptly (simulates SSH drop).
        proc.kill()
        proc.wait(timeout=5)

        # Relay daemon should still be running.
        _wait_for_socket(sock_path, timeout=3)

        # Reconnect as new client.
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.connect(sock_path)

        # Send handshake.
        hs = json.dumps({"offset": 0}) + "\n"
        conn.sendall(hs.encode())

        # Read replayed data.
        time.sleep(0.3)
        data = conn.recv(65536)
        assert b"test\n" in data, f"expected replayed 'test\\n', got: {data!r}"

        # Now send null byte to trigger graceful shutdown.
        conn.sendall(b"\x00")
        conn.close()

        # Wait for daemon to exit.
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            if not os.path.exists(pid_path):
                break
            try:
                with open(pid_path) as f:
                    pid = int(f.read().strip())
                os.kill(pid, 0)
            except OSError:
                break
            time.sleep(0.1)
        else:
            raise AssertionError("relay daemon did not exit after close-stdin sentinel")
    finally:
        _cleanup(relay_dir)


if __name__ == "__main__":
    print("test_close_stdin_sentinel...", end=" ", flush=True)
    test_close_stdin_sentinel()
    print("OK")

    print("test_ssh_drop_keeps_subprocess...", end=" ", flush=True)
    test_ssh_drop_keeps_subprocess()
    print("OK")

    print("All tests passed.")
