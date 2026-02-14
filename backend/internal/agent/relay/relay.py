#!/usr/bin/env python3
# Persistent relay for claude processes inside caic containers.
#
# Two modes:
#   serve-attach -- <cmd...>   Start relay server + attach as first client.
#   attach [--offset N]        Reconnect to an existing relay server.
#
# The relay server owns the subprocess stdin/stdout, logs all I/O to a
# file (output.jsonl), and accepts client connections via a Unix socket.
# When a client disconnects (SSH drops), the subprocess keeps running.

import json
import os
import socket
import subprocess
import sys
import threading
import time

RELAY_DIR = "/tmp/caic-relay"
SOCK_PATH = os.path.join(RELAY_DIR, "relay.sock")
OUTPUT_PATH = os.path.join(RELAY_DIR, "output.jsonl")
PID_PATH = os.path.join(RELAY_DIR, "pid")

# Max size of a single read from subprocess stdout.
BUF_SIZE = 65536


def serve(cmd_args, work_dir):
    """Start the relay server as a daemon, then attach as the first client."""
    os.makedirs(RELAY_DIR, exist_ok=True)

    # Clean up stale socket.
    try:
        os.unlink(SOCK_PATH)
    except FileNotFoundError:
        pass

    # Fork to become a daemon.
    pid = os.fork()
    if pid > 0:
        # Parent: wait for socket to appear, then attach.
        _wait_for_socket(30)
        attach_client(offset=0)
        return

    # Child: become session leader so we survive SSH disconnects.
    os.setsid()

    # Redirect our own stderr to a log file for debugging.
    log_fd = os.open(
        os.path.join(RELAY_DIR, "relay.log"),
        os.O_WRONLY | os.O_CREAT | os.O_TRUNC,
        0o644,
    )
    os.dup2(log_fd, 2)
    os.close(log_fd)

    # Write PID file.
    with open(PID_PATH, "w") as f:
        f.write(str(os.getpid()))

    # Change to the working directory before starting the subprocess.
    os.chdir(work_dir)

    # Start the subprocess.
    proc = subprocess.Popen(
        cmd_args,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )

    # Open output log (append-only).
    output_file = open(OUTPUT_PATH, "ab", buffering=0)

    # Track connected client (at most one at a time).
    client_lock = threading.Lock()
    client_conn = [None]  # mutable ref in list

    def set_client(conn):
        with client_lock:
            old = client_conn[0]
            client_conn[0] = conn
            if old is not None:
                try:
                    old.close()
                except OSError:
                    pass

    def send_to_client(data):
        with client_lock:
            c = client_conn[0]
            if c is None:
                return
            try:
                c.sendall(data)
            except (BrokenPipeError, ConnectionResetError, OSError):
                client_conn[0] = None

    # Thread: read subprocess stdout → log + forward to client.
    def reader_thread():
        try:
            while True:
                data = proc.stdout.read1(BUF_SIZE)
                if not data:
                    break
                output_file.write(data)
                output_file.flush()
                send_to_client(data)
        except (OSError, ValueError):
            pass
        finally:
            output_file.close()
            # Process exited — close client.
            set_client(None)

    t = threading.Thread(target=reader_thread, daemon=True)
    t.start()

    # Listen on Unix socket for client connections.
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(SOCK_PATH)
    srv.listen(1)

    # Thread: accept client connections.
    def accept_thread():
        while True:
            try:
                conn, _ = srv.accept()
            except OSError:
                break

            # Read handshake: {"offset": N}\n
            try:
                hdr = _read_line(conn)
                hs = json.loads(hdr)
                offset = hs.get("offset", 0)
            except (json.JSONDecodeError, OSError, ValueError):
                conn.close()
                continue

            # Replay output from offset.
            try:
                with open(OUTPUT_PATH, "rb") as f:
                    f.seek(offset)
                    while True:
                        chunk = f.read(BUF_SIZE)
                        if not chunk:
                            break
                        conn.sendall(chunk)
            except (OSError, BrokenPipeError):
                conn.close()
                continue

            set_client(conn)

            # Thread: read client stdin → subprocess stdin + log.
            def client_reader(c):
                try:
                    while True:
                        data = c.recv(BUF_SIZE)
                        if not data:
                            break
                        proc.stdin.write(data)
                        proc.stdin.flush()
                        output_file.write(data)
                        output_file.flush()
                except (OSError, BrokenPipeError, ValueError):
                    pass

            ct = threading.Thread(target=client_reader, args=(conn,), daemon=True)
            ct.start()

    at = threading.Thread(target=accept_thread, daemon=True)
    at.start()

    # Wait for subprocess to exit.
    proc.wait()
    t.join(timeout=5)

    # Clean up.
    srv.close()
    try:
        os.unlink(SOCK_PATH)
    except FileNotFoundError:
        pass
    try:
        os.unlink(PID_PATH)
    except FileNotFoundError:
        pass


def attach_client(offset):
    """Connect to relay via Unix socket and bridge to stdio."""
    conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    conn.connect(SOCK_PATH)

    # Send handshake.
    hs = json.dumps({"offset": offset}) + "\n"
    conn.sendall(hs.encode())

    # Thread: relay socket → stdout.
    def relay_to_stdout():
        try:
            while True:
                data = conn.recv(BUF_SIZE)
                if not data:
                    break
                sys.stdout.buffer.write(data)
                sys.stdout.buffer.flush()
        except (OSError, BrokenPipeError, ValueError):
            pass
        finally:
            # When relay closes, signal EOF to our parent.
            try:
                sys.stdout.close()
            except OSError:
                pass

    t = threading.Thread(target=relay_to_stdout, daemon=True)
    t.start()

    # Main thread: stdin → socket.
    try:
        while True:
            data = sys.stdin.buffer.read1(BUF_SIZE)
            if not data:
                break
            conn.sendall(data)
    except (OSError, BrokenPipeError, ValueError, KeyboardInterrupt):
        pass
    finally:
        conn.close()
        t.join(timeout=5)


def _wait_for_socket(timeout):
    """Block until the relay socket appears."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if os.path.exists(SOCK_PATH):
            # Try connecting to verify it's ready.
            try:
                s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                s.connect(SOCK_PATH)
                s.close()
                return
            except OSError:
                pass
        time.sleep(0.05)
    print("relay: timed out waiting for socket", file=sys.stderr)
    sys.exit(1)


def _read_line(conn):
    """Read bytes from conn until newline."""
    buf = bytearray()
    while True:
        b = conn.recv(1)
        if not b or b == b"\n":
            break
        buf.extend(b)
    return buf.decode()


def read_plan(path):
    """Print the content of a plan file.

    If path is given, read that file directly. Otherwise find the most recently
    modified .md file in ~/.claude/plans/.
    """
    if path:
        with open(path) as f:
            sys.stdout.write(f.read())
        return
    plans_dir = os.path.expanduser("~/.claude/plans")
    if not os.path.isdir(plans_dir):
        sys.exit(1)
    files = [os.path.join(plans_dir, f) for f in os.listdir(plans_dir) if f.endswith(".md")]
    if not files:
        sys.exit(1)
    latest = max(files, key=os.path.getmtime)
    with open(latest) as f:
        sys.stdout.write(f.read())


def main():
    if len(sys.argv) < 2:
        print("usage: relay.py serve-attach --dir <path> -- <cmd...>", file=sys.stderr)
        print("       relay.py attach [--offset N]", file=sys.stderr)
        print("       relay.py read-plan [path]", file=sys.stderr)
        sys.exit(1)

    mode = sys.argv[1]

    if mode == "serve-attach":
        # Parse required --dir flag.
        rest = sys.argv[2:]
        if len(rest) < 2 or rest[0] != "--dir":
            print("relay.py serve-attach: --dir <path> is required", file=sys.stderr)
            sys.exit(1)
        work_dir = rest[1]
        rest = rest[2:]
        # Find "--" separator.
        try:
            sep = rest.index("--")
        except ValueError:
            print("relay.py serve-attach: missing '--' separator", file=sys.stderr)
            sys.exit(1)
        cmd_args = rest[sep + 1 :]
        if not cmd_args:
            print("relay.py serve-attach: no command after '--'", file=sys.stderr)
            sys.exit(1)
        serve(cmd_args, work_dir)

    elif mode == "attach":
        offset = 0
        if "--offset" in sys.argv:
            idx = sys.argv.index("--offset")
            if idx + 1 < len(sys.argv):
                offset = int(sys.argv[idx + 1])
        attach_client(offset)

    elif mode == "read-plan":
        read_plan(sys.argv[2] if len(sys.argv) > 2 else None)

    else:
        print(f"relay.py: unknown mode {mode!r}", file=sys.stderr)
        sys.exit(1)


if __name__ == "__main__":
    main()
