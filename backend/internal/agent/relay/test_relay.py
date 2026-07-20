#!/usr/bin/env python3
"""Tests for relay.py lifecycle: shutdown semantics, signal escalation, and timing."""

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


def _wait_for_daemon_exit(pid_path, timeout=15):
    """Wait for the relay daemon to exit by polling its PID file."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not os.path.exists(pid_path):
            return
        try:
            with open(pid_path) as f:
                pid = int(f.read().strip())
            os.kill(pid, 0)
        except OSError:
            return
        time.sleep(0.1)
    raise AssertionError(f"relay daemon did not exit within {timeout}s")


def _read_relay_log(relay_dir):
    """Return relay.log contents or empty string."""
    try:
        with open(os.path.join(relay_dir, "relay.log")) as f:
            return f.read()
    except FileNotFoundError:
        return ""


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

    # Python script that handles SIGINT gracefully (like a real agent),
    # reads stdin to a file, then writes a DONE marker on exit.
    script = os.path.join(relay_dir, "test.py")
    with open(script, "w") as f:
        f.write(
            f"import signal, sys\n"
            f"signal.signal(signal.SIGINT, lambda *_: None)\n"
            f"with open({out_path!r}, 'wb') as f:\n"
            f"    for line in sys.stdin.buffer:\n"
            f"        f.write(line)\n"
            f"with open({out_path!r}, 'a') as f:\n"
            f"    f.write('DONE\\n')\n"
        )

    try:
        proc = subprocess.Popen(
            [sys.executable, RELAY_PY, "serve-attach", "--dir", relay_dir, "--", sys.executable, script],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        # Send data.
        proc.stdin.write(b"hello\n")
        proc.stdin.flush()
        time.sleep(0.3)

        # Send explicit null-byte sentinel, then close stdin.
        # The relay no longer infers shutdown from stdin EOF alone.
        proc.stdin.write(b"\x00\n")
        proc.stdin.flush()
        proc.stdin.close()

        # Attach client should exit, then daemon should exit (subprocess gets EOF).
        proc.wait(timeout=15)

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


def test_stdin_eof_keeps_subprocess():
    """Closing attach client's stdin (without null byte) must NOT close
    proc.stdin — the subprocess stays alive. This simulates an SSH drop
    where the attach client sees EOF on stdin."""
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

        # Close stdin WITHOUT sending null byte — simulates SSH drop at the
        # attach_client level (stdin EOF without sentinel).
        proc.stdin.close()

        # Attach client should exit.
        proc.wait(timeout=10)

        # Relay daemon should still be running.
        _wait_for_socket(sock_path, timeout=3)

        # Reconnect and verify subprocess is alive.
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(5.0)
        conn.connect(sock_path)
        hs = json.dumps({"offset": 0}) + "\n"
        conn.sendall(hs.encode())
        time.sleep(0.3)
        data = conn.recv(65536)
        assert b"test\n" in data, f"expected replayed 'test\\n', got: {data!r}"

        # Send null byte to trigger graceful shutdown.
        conn.sendall(b"\x00\n")
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
            raise AssertionError("relay daemon did not exit")
    finally:
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

        # Wait until output.jsonl contains the echoed data before killing.
        # proc.kill() sends SIGKILL, which may race with the attach_client
        # forwarding the data from its stdin pipe to the daemon socket.  If we
        # kill before the forward completes, the data is lost and output.jsonl
        # stays empty, causing conn.recv() below to block indefinitely.
        output_path = os.path.join(relay_dir, "output.jsonl")
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                with open(output_path, "rb") as f:
                    if b"test\n" in f.read():
                        break
            except FileNotFoundError:
                pass
            time.sleep(0.05)
        else:
            raise AssertionError("data did not appear in output.jsonl before kill")

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
        conn.settimeout(5.0)
        data = conn.recv(65536)
        assert b"test\n" in data, f"expected replayed 'test\\n', got: {data!r}"

        # Now send null byte to trigger graceful shutdown.
        conn.sendall(b"\x00\n")
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


def test_shutdown_kills_stuck_subprocess():
    """When the subprocess ignores stdin close, the relay escalates to
    SIGTERM after _SHUTDOWN_GRACE seconds, forcing exit."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    pid_path = os.path.join(relay_dir, "pid")
    env = _make_env(relay_dir)

    # Script that ignores stdin EOF and loops forever.
    script = os.path.join(relay_dir, "stuck.sh")
    with open(script, "w") as f:
        f.write("#!/bin/sh\ncat > /dev/null\nwhile true; do sleep 1; done\n")
    os.chmod(script, 0o755)

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--shutdown-grace",
                "2",
                "--",
                "/bin/sh",
                script,
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        time.sleep(0.5)

        # Send null byte sentinel to request shutdown.
        proc.stdin.write(b"\x00\n")
        proc.stdin.flush()
        proc.stdin.close()

        # Relay daemon should escalate and kill the stuck subprocess.
        # --shutdown-grace=2s + 2s SIGTERM grace + margin.
        # The attach_client may block in t.join(timeout=25) until the relay
        # daemon kills the subprocess and closes the socket.
        deadline = time.monotonic() + 15
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
            raise AssertionError("relay daemon did not exit after killing stuck subprocess")

        # Attach client should also have exited by now.
        proc.wait(timeout=5)
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_shutdown_timing_fast_subprocess():
    """A subprocess that exits immediately on stdin EOF should cause the relay
    to shut down within 2 seconds — no grace period wasted."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    env = _make_env(relay_dir)

    try:
        # "cat" exits as soon as stdin closes — the ideal fast case.
        proc = subprocess.Popen(
            [sys.executable, RELAY_PY, "serve-attach", "--dir", relay_dir, "--shutdown-grace", "10", "--", "cat"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        proc.stdin.write(b"hello\n")
        proc.stdin.flush()
        time.sleep(0.3)

        t0 = time.monotonic()
        proc.stdin.write(b"\x00\n")
        proc.stdin.flush()
        proc.stdin.close()
        proc.wait(timeout=10)
        elapsed = time.monotonic() - t0

        assert elapsed < 2.0, (
            f"fast subprocess shutdown took {elapsed:.1f}s (expected <2s).\nrelay.log:\n{_read_relay_log(relay_dir)}"
        )
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_shutdown_timing_sigint_respected():
    """A subprocess that exits on SIGINT (like Claude Code) should cause the
    relay to shut down promptly — well within the grace period."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    env = _make_env(relay_dir)

    # Script: ignores stdin EOF (reads to /dev/null then loops), but exits on SIGINT.
    script = os.path.join(relay_dir, "sigint_exit.py")
    with open(script, "w") as f:
        f.write(
            "import signal, sys, time\n"
            "def handler(sig, frame):\n"
            "    sys.exit(0)\n"
            "signal.signal(signal.SIGINT, handler)\n"
            "sys.stdin.read()  # drain stdin\n"
            "while True:\n"
            "    time.sleep(1)\n"
        )

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--shutdown-grace",
                "10",
                "--",
                sys.executable,
                script,
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        time.sleep(0.5)

        t0 = time.monotonic()
        proc.stdin.write(b"\x00\n")
        proc.stdin.flush()
        proc.stdin.close()
        proc.wait(timeout=15)
        elapsed = time.monotonic() - t0

        assert elapsed < 2.0, (
            f"SIGINT-responsive subprocess shutdown took {elapsed:.1f}s (expected <2s).\n"
            f"relay.log:\n{_read_relay_log(relay_dir)}"
        )

        log = _read_relay_log(relay_dir)
        assert "sigint" in log.lower(), f"relay log should mention SIGINT.\nrelay.log:\n{log}"
        assert "sigterm" not in log.lower(), f"should NOT have escalated to SIGTERM.\nrelay.log:\n{log}"
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_shutdown_sigterm_escalation_timing():
    """When subprocess ignores SIGINT, relay must escalate to SIGTERM after
    exactly --shutdown-grace seconds (not sooner, not much later)."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    pid_path = os.path.join(relay_dir, "pid")
    env = _make_env(relay_dir)
    grace = 2

    # Script that ignores SIGINT but exits on SIGTERM (default behavior).
    script = os.path.join(relay_dir, "ignore_sigint.py")
    with open(script, "w") as f:
        f.write(
            "import signal, sys, time\n"
            "signal.signal(signal.SIGINT, signal.SIG_IGN)\n"
            "sys.stdin.read()\n"
            "while True:\n"
            "    time.sleep(1)\n"
        )

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--shutdown-grace",
                str(grace),
                "--",
                sys.executable,
                script,
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        time.sleep(0.5)

        t0 = time.monotonic()
        proc.stdin.write(b"\x00\n")
        proc.stdin.flush()
        proc.stdin.close()

        _wait_for_daemon_exit(pid_path, timeout=grace + 5)
        elapsed = time.monotonic() - t0

        # Should take at least grace seconds (SIGINT wait) but not much more.
        assert elapsed >= grace, f"shutdown took {elapsed:.1f}s, expected >= {grace}s (SIGINT grace period)"
        assert elapsed < grace + 3, (
            f"shutdown took {elapsed:.1f}s, expected < {grace + 3}s.\nrelay.log:\n{_read_relay_log(relay_dir)}"
        )

        log = _read_relay_log(relay_dir)
        assert "sigterm" in log.lower(), f"relay should have escalated to SIGTERM.\nrelay.log:\n{log}"
        assert "sigkill" not in log.lower(), f"should NOT have needed SIGKILL.\nrelay.log:\n{log}"
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_shutdown_sigkill_escalation():
    """When subprocess ignores both SIGINT and SIGTERM, relay escalates to SIGKILL."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    pid_path = os.path.join(relay_dir, "pid")
    env = _make_env(relay_dir)
    grace = 1

    # Script that ignores both SIGINT and SIGTERM.
    script = os.path.join(relay_dir, "unkillable.py")
    with open(script, "w") as f:
        f.write(
            "import signal, sys, time\n"
            "signal.signal(signal.SIGINT, signal.SIG_IGN)\n"
            "signal.signal(signal.SIGTERM, signal.SIG_IGN)\n"
            "sys.stdin.read()\n"
            "while True:\n"
            "    time.sleep(1)\n"
        )

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--shutdown-grace",
                str(grace),
                "--",
                sys.executable,
                script,
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        time.sleep(0.5)

        t0 = time.monotonic()
        proc.stdin.write(b"\x00\n")
        proc.stdin.flush()
        proc.stdin.close()

        # grace (SIGINT wait) + 2 (SIGTERM wait) + margin
        _wait_for_daemon_exit(pid_path, timeout=grace + 7)
        elapsed = time.monotonic() - t0

        assert elapsed >= grace + 2, f"shutdown took {elapsed:.1f}s, expected >= {grace + 2}s (SIGINT + SIGTERM grace)"
        assert elapsed < grace + 7, f"shutdown took {elapsed:.1f}s, too slow.\nrelay.log:\n{_read_relay_log(relay_dir)}"

        log = _read_relay_log(relay_dir)
        assert "sigkill" in log.lower(), f"relay should have escalated to SIGKILL.\nrelay.log:\n{log}"
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_attach_client_receives_final_output():
    """After sending the null-byte sentinel, the attach client should still
    receive any final output the subprocess emits before exiting."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    env = _make_env(relay_dir)

    # Script that writes a farewell message to stdout on SIGINT, then exits.
    script = os.path.join(relay_dir, "farewell.py")
    with open(script, "w") as f:
        f.write(
            "import signal, sys, time\n"
            "def handler(sig, frame):\n"
            "    sys.stdout.write('FAREWELL\\n')\n"
            "    sys.stdout.flush()\n"
            "    sys.exit(0)\n"
            "signal.signal(signal.SIGINT, handler)\n"
            "sys.stdout.write('READY\\n')\n"
            "sys.stdout.flush()\n"
            "sys.stdin.read()\n"
            "while True:\n"
            "    time.sleep(1)\n"
        )

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--shutdown-grace",
                "5",
                "--",
                sys.executable,
                script,
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        # Wait for READY.
        deadline = time.monotonic() + 10
        stdout_data = b""
        while time.monotonic() < deadline:
            chunk = proc.stdout.read1(4096)
            if chunk:
                stdout_data += chunk
            if b"READY" in stdout_data:
                break
            time.sleep(0.1)
        assert b"READY" in stdout_data, f"subprocess did not emit READY, got: {stdout_data!r}"

        # Send sentinel.
        proc.stdin.write(b"\x00\n")
        proc.stdin.flush()
        proc.stdin.close()

        # Read remaining stdout — should include FAREWELL.
        proc.wait(timeout=10)
        remaining = proc.stdout.read()
        stdout_data += remaining

        assert b"FAREWELL" in stdout_data, (
            f"attach client should have received FAREWELL, got: {stdout_data!r}\n"
            f"relay.log:\n{_read_relay_log(relay_dir)}"
        )
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_reconnect_after_ssh_drop_then_shutdown():
    """Full lifecycle: data → SSH drop → reconnect → more data → graceful shutdown.

    Verifies output.jsonl replay includes all data across both sessions."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    sock_path = os.path.join(relay_dir, "relay.sock")
    output_path = os.path.join(relay_dir, "output.jsonl")
    pid_path = os.path.join(relay_dir, "pid")
    env = _make_env(relay_dir)

    try:
        # Start relay with cat subprocess.
        proc = subprocess.Popen(
            [sys.executable, RELAY_PY, "serve-attach", "--dir", relay_dir, "--", "cat"],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )

        # Session 1: send data.
        proc.stdin.write(b"first\n")
        proc.stdin.flush()

        # Wait for data to appear in output.jsonl.
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            try:
                with open(output_path, "rb") as f:
                    if b"first\n" in f.read():
                        break
            except FileNotFoundError:
                pass
            time.sleep(0.05)

        # Kill attach client (simulate SSH drop — no sentinel).
        proc.kill()
        proc.wait(timeout=5)

        # Daemon should survive.
        _wait_for_socket(sock_path, timeout=3)

        # Session 2: reconnect via raw socket.
        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(5.0)
        conn.connect(sock_path)
        conn.sendall(json.dumps({"offset": 0}).encode() + b"\n")

        # Read replay — should contain "first".
        replay = conn.recv(65536)
        assert b"first\n" in replay, f"replay should contain 'first', got: {replay!r}"

        # Send more data through session 2.
        conn.sendall(b"second\n")
        time.sleep(0.3)

        # Read echoed "second" from cat.
        echoed = conn.recv(65536)
        assert b"second\n" in echoed, f"expected echoed 'second', got: {echoed!r}"

        # Graceful shutdown via session 2.
        conn.sendall(b"\x00\n")
        conn.close()

        _wait_for_daemon_exit(pid_path, timeout=10)

        # Verify output.jsonl has both pieces.
        with open(output_path, "rb") as f:
            full_output = f.read()
        assert b"first\n" in full_output, "output.jsonl missing 'first'"
        assert b"second\n" in full_output, "output.jsonl missing 'second'"
    finally:
        _cleanup(relay_dir)


def test_serve_discards_previous_session_output():
    """A new serve session must not replay a prior agent's terminal event."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    output_path = os.path.join(relay_dir, "output.jsonl")
    env = _make_env(relay_dir)
    with open(output_path, "w") as f:
        f.write('{"type":"caic_exit","exit_code":1,"error":"stale failure"}\n')

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--",
                sys.executable,
                "-c",
                "print('fresh startup')",
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        stdout, _ = proc.communicate(timeout=10)

        assert b"fresh startup" in stdout, stdout
        assert b"stale failure" not in stdout, stdout
        with open(output_path, "rb") as f:
            output = f.read()
        assert b"fresh startup" in output, output
        assert b"stale failure" not in output, output
    finally:
        _cleanup(relay_dir)


def test_caic_exit_includes_stderr():
    """A subprocess stderr failure is recorded in caic_exit.error."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    output_path = os.path.join(relay_dir, "output.jsonl")
    env = _make_env(relay_dir)

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--",
                sys.executable,
                "-c",
                "import sys; print('Unknown option: --approve', file=sys.stderr); sys.exit(2)",
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        proc.stdin.close()
        proc.wait(timeout=10)

        with open(output_path) as f:
            lines = [json.loads(line) for line in f if line.strip()]
        exit_events = [line for line in lines if line.get("type") == "caic_exit"]
        assert exit_events, f"output.jsonl missing caic_exit: {lines!r}"
        event = exit_events[-1]
        assert event["exit_code"] == 2, event
        assert event["cmd"][:2] == [sys.executable, "-c"], event
        assert "Unknown option: --approve" in event.get("error", ""), event
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        _cleanup(relay_dir)


def test_popen_failure_writes_caic_exit():
    """A command that cannot be spawned still produces a structured caic_exit."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    output_path = os.path.join(relay_dir, "output.jsonl")
    env = _make_env(relay_dir)
    proc = None

    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                RELAY_PY,
                "serve-attach",
                "--dir",
                relay_dir,
                "--",
                os.path.join(relay_dir, "missing-agent"),
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        stdout, _ = proc.communicate(timeout=10)

        with open(output_path) as f:
            lines = [json.loads(line) for line in f if line.strip()]
        exit_events = [line for line in lines if line.get("type") == "caic_exit"]
        assert exit_events, f"output.jsonl missing caic_exit: {lines!r}"
        event = exit_events[-1]
        assert event["exit_code"] == -1, event
        assert "missing-agent" in event.get("error", ""), event
        assert b"caic_exit" in stdout, stdout
        assert b"missing-agent" in stdout, stdout
    finally:
        if proc is not None:
            try:
                proc.kill()
            except OSError:
                pass
        _cleanup(relay_dir)


def test_parse_numstat():
    """Test _parse_numstat parses git diff --numstat output correctly."""
    # Import the module under test.
    import importlib.util

    spec = importlib.util.spec_from_file_location("relay", RELAY_PY)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)

    # Normal files.
    result = mod._parse_numstat("10\t3\tsrc/main.go\n5\t0\tREADME.md\n")
    assert len(result) == 2, f"expected 2 files, got {len(result)}"
    assert result[0] == {"path": "src/main.go", "added": 10, "deleted": 3}
    assert result[1] == {"path": "README.md", "added": 5, "deleted": 0}

    # Binary file.
    result = mod._parse_numstat("-\t-\timage.png\n")
    assert len(result) == 1
    assert result[0] == {"path": "image.png", "added": 0, "deleted": 0, "binary": True}

    # Empty input.
    result = mod._parse_numstat("")
    assert result == []

    # Mixed.
    result = mod._parse_numstat("1\t2\ta.txt\n-\t-\tb.bin\n3\t0\tc.rs\n")
    assert len(result) == 3
    assert result[0]["path"] == "a.txt"
    assert result[1]["binary"] is True
    assert result[2]["added"] == 3


if __name__ == "__main__":
    tests = [
        test_parse_numstat,
        test_close_stdin_sentinel,
        test_stdin_eof_keeps_subprocess,
        test_ssh_drop_keeps_subprocess,
        test_shutdown_kills_stuck_subprocess,
        test_shutdown_timing_fast_subprocess,
        test_shutdown_timing_sigint_respected,
        test_shutdown_sigterm_escalation_timing,
        test_shutdown_sigkill_escalation,
        test_attach_client_receives_final_output,
        test_reconnect_after_ssh_drop_then_shutdown,
        test_caic_exit_includes_stderr,
        test_popen_failure_writes_caic_exit,
    ]
    failed = []
    for t in tests:
        name = t.__name__
        print(f"{name}...", end=" ", flush=True)
        try:
            t()
            print("OK")
        except Exception as e:
            print(f"FAIL: {e}")
            failed.append(name)

    if failed:
        print(f"\n{len(failed)} FAILED: {', '.join(failed)}")
        sys.exit(1)
    print(f"\nAll {len(tests)} tests passed.")
