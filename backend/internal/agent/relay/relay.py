#!/usr/bin/env python3
# Persistent relay for coding agent processes inside caic containers.
#
# Modes:
#   serve-attach --dir <path> -- <cmd...>   Start relay daemon + attach as first client.
#   attach [--offset N]                     Reconnect to a running relay daemon.
#   read-plan [path]                        Read a plan file from the container.
#
# The relay daemon owns the subprocess stdin/stdout, logs all I/O to
# output.jsonl, and accepts one client at a time via a Unix socket.
#
# Shutdown protocol — null-byte sentinel line:
#   The Go backend (Session.SendStop) writes \x00\n to stdin. The
#   attach_client forwards this through the socket to the daemon, whose
#   _client_reader detects the sentinel and sets shutdown_event. The
#   _shutdown_watchdog thread waits on this event, then closes proc.stdin,
#   sends SIGINT, and escalates to SIGTERM/SIGKILL after --shutdown-grace.
#   reader_thread is the authoritative "done" signal: it blocks on
#   subprocess stdout until EOF, then closes the client socket.
#   serve() joins reader_thread and cleans up.
#
#   Crucially, stdin EOF alone does NOT trigger shutdown. This is what
#   distinguishes the two flows below.
#
# Flow 1 — One task is purged (user clicks "purge"):
#   1. Server calls Runner.Cleanup → Session.Stop writes \x00\n
#   2. attach_client forwards \x00\n through the socket
#   3. _client_reader detects sentinel, sets shutdown_event
#   4. _shutdown_watchdog closes proc.stdin, sends SIGINT; if subprocess
#      doesn't exit within --shutdown-grace, escalates to SIGTERM/SIGKILL
#   5. reader_thread sees stdout EOF, closes client socket
#   6. Server kills the container
#
# Flow 2 — Backend restarts (upgrade, crash):
#   1. SSH connections are severed, attach_client sees stdin EOF
#   2. attach_client disconnects from the socket (no \x00 sent)
#   3. Relay daemon stays alive, subprocess keeps running
#   4. On restart, server discovers the container via adoptOne()
#   5. Server reads output.jsonl to restore conversation state
#   6. Server calls relay.py attach --offset N to reconnect
#   7. Task resumes seamlessly with zero message loss

import argparse
import json
import logging
import os
import signal
import socket
import subprocess
import sys
import threading
import time

RELAY_DIR = os.environ.get("CAIC_RELAY_DIR", "/tmp/caic-relay")
SOCK_PATH = os.path.join(RELAY_DIR, "relay.sock")
OUTPUT_PATH = os.path.join(RELAY_DIR, "output.jsonl")
PID_PATH = os.path.join(RELAY_DIR, "pid")

# Max size of a single read from subprocess stdout.
BUF_SIZE = 65536

# Interval between diff stat polls (seconds).
_DIFF_THROTTLE = 10  # minimum seconds between diff runs
_DIFF_DEBOUNCE = 2  # seconds of quiet before running diff

# Default grace period (seconds) after SIGINT before escalating to
# SIGTERM/SIGKILL. Overridable via --shutdown-grace.
_DEFAULT_SHUTDOWN_GRACE = 10


def _parse_numstat(numstat):
    """Parse git diff --numstat output into a list of file stat dicts.

    Each line has the format: <added>\\t<deleted>\\t<path>.
    Binary files use "-\\t-\\t<path>".
    Returns an empty list if there are no changed files.
    """
    result = []
    for line in numstat.strip().splitlines():
        line = line.strip()
        if not line:
            continue
        parts = line.split("\t", 2)
        if len(parts) != 3:
            continue
        added_str, deleted_str, path = parts
        if added_str == "-" and deleted_str == "-":
            result.append({"path": path, "added": 0, "deleted": 0, "binary": True})
        else:
            try:
                added = int(added_str)
            except ValueError:
                added = 0
            try:
                deleted = int(deleted_str)
            except ValueError:
                deleted = 0
            result.append({"path": path, "added": added, "deleted": deleted})
    return result


class _Daemon:
    """Relay daemon state and thread entry points.

    Encapsulates shared mutable state that was previously captured via
    closures inside serve(). Each thread method operates on explicit
    ``self`` attributes instead.
    """

    def __init__(self, proc, output_file, work_dir, log_stdin, env_event):
        self.proc = proc
        self.output_file = output_file
        self.work_dir = work_dir
        self.log_stdin = log_stdin
        self.env_event = env_event  # Pre-encoded caic_stripped_env NDJSON; b"" when nothing was stripped.

        self.output_lock = threading.Lock()
        self.client_lock = threading.Lock()
        self.client_conn = None
        self.client_id = 0
        self.shutdown_event = threading.Event()
        self.diff_activity = threading.Event()

    def set_client(self, conn, reason=""):
        with self.client_lock:
            old = self.client_conn
            self.client_conn = conn
            if old is not None:
                logging.info("client #%d disconnected reason=%s", self.client_id, reason)
                try:
                    old.close()
                except OSError:
                    pass

    def send_to_client(self, data):
        with self.client_lock:
            c = self.client_conn
            if c is None:
                return
            try:
                c.sendall(data)
            except (BrokenPipeError, ConnectionResetError, OSError):
                self.client_conn = None

    def write_output(self, *chunks):
        """Write one or more byte chunks to output.jsonl under the lock."""
        with self.output_lock:
            for chunk in chunks:
                self.output_file.write(chunk)
            self.output_file.flush()

    # -- threads -------------------------------------------------------------

    def reader_thread(self):
        """Read subprocess stdout → output.jsonl + connected client."""
        try:
            while True:
                data = self.proc.stdout.read1(BUF_SIZE)
                if not data:
                    break
                # Inject caic_stripped_env after the first subprocess output
                # (system/init) which confirms auth succeeded.
                ev = self.env_event
                self.env_event = b""
                if ev:
                    self.write_output(data, ev)
                else:
                    self.write_output(data)
                self.send_to_client(data)
                if ev:
                    self.send_to_client(ev)
                self.diff_activity.set()
        except (OSError, ValueError) as e:
            logging.warning("reader_thread error: %s", e)
        finally:
            sz = self.output_file.tell() if not self.output_file.closed else -1
            self.output_file.close()
            self.set_client(None, "subprocess_eof")
            self.diff_activity.set()
            logging.info("reader_thread exited output_bytes=%d proc_poll=%s", sz, self.proc.poll())

    def diff_watcher_thread(self):
        """Poll git diff on activity, with throttle + debounce.

        Uses a temporary index to include untracked files without mutating
        the real index. Diffs against "base" (merge-base ref set by md
        start); falls back to HEAD if "base" doesn't exist.
        """
        tmp_index = os.path.join(RELAY_DIR, "diff.index")
        diff_env = {**os.environ, "GIT_INDEX_FILE": tmp_index}
        prev_raw = None
        last_run = 0.0
        diff_ref = "base"
        try:
            cp = subprocess.run(
                ["git", "rev-parse", "--verify", "base"],
                cwd=self.work_dir,
                capture_output=True,
                timeout=5,
            )
            if cp.returncode != 0:
                diff_ref = "HEAD"
        except (subprocess.TimeoutExpired, OSError):
            diff_ref = "HEAD"
        while self.proc.poll() is None:
            if not self.diff_activity.wait(timeout=30):
                continue
            self.diff_activity.clear()
            if self.proc.poll() is not None:
                break
            # Debounce: wait for quiet period (no new activity).
            while True:
                if self.diff_activity.wait(timeout=_DIFF_DEBOUNCE):
                    self.diff_activity.clear()
                    if self.proc.poll() is not None:
                        break
                else:
                    break
            if self.proc.poll() is not None:
                break
            # Throttle: enforce minimum interval.
            now = time.monotonic()
            wait = _DIFF_THROTTLE - (now - last_run)
            if wait > 0:
                time.sleep(wait)
                if self.proc.poll() is not None:
                    break
            last_run = time.monotonic()
            try:
                subprocess.run(
                    ["git", "read-tree", diff_ref],
                    cwd=self.work_dir,
                    env=diff_env,
                    capture_output=True,
                    timeout=5,
                )
                subprocess.run(
                    ["git", "add", "--intent-to-add", "--all"],
                    cwd=self.work_dir,
                    env=diff_env,
                    capture_output=True,
                    timeout=5,
                )
                cp = subprocess.run(
                    ["git", "diff", "--numstat", diff_ref],
                    cwd=self.work_dir,
                    env=diff_env,
                    capture_output=True,
                    text=True,
                    timeout=5,
                )
                raw = cp.stdout
            except (subprocess.TimeoutExpired, OSError):
                continue
            if raw == prev_raw:
                continue
            prev_raw = raw
            diff_stat = _parse_numstat(raw)
            line = json.dumps({"type": "caic_diff_stat", "diff_stat": diff_stat, "ts": time.time()}) + "\n"
            encoded = line.encode()
            try:
                self.write_output(encoded)
            except (OSError, ValueError):
                pass
            self.send_to_client(encoded)

    def accept_thread(self, srv):
        """Accept client connections on the Unix socket."""
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

            self.client_id += 1
            cid = self.client_id
            self.set_client(conn, "replaced")
            logging.info(
                "client #%d connected offset=%d shutting_down=%s proc_alive=%s",
                cid,
                offset,
                self.shutdown_event.is_set(),
                self.proc.poll() is None,
            )

            ct = threading.Thread(target=self._client_reader, args=(conn, cid), daemon=True)
            ct.start()

    def _client_reader(self, c, cid):
        """Read client stdin → subprocess stdin + output log.

        User input is NDJSON: each message is a single JSON line ending
        with newline. Large messages (e.g. base64 images) may span multiple
        recv() calls. We buffer incoming data and process complete lines:
        each line is forwarded to proc.stdin and logged to output_file.
        Incomplete trailing data is held until the next recv completes it.

        A line consisting of a single null byte (``\\x00\\n``, sent by
        Session.SendStop) is the graceful-shutdown sentinel. It is consumed
        here and never forwarded to the subprocess.
        """
        close_stdin = False
        buf = bytearray()
        try:
            while not close_stdin:
                data = c.recv(BUF_SIZE)
                if not data:
                    logging.info("client #%d recv EOF", cid)
                    break
                buf += data
                while (nl := buf.find(b"\n")) >= 0:
                    line = bytes(buf[:nl])
                    del buf[: nl + 1]
                    if line == b"\x00":
                        logging.info("client #%d received shutdown sentinel", cid)
                        close_stdin = True
                        break
                    payload = line + b"\n"
                    self.proc.stdin.write(payload)
                    self.proc.stdin.flush()
                    if self.log_stdin:
                        self.write_output(payload)
        except (OSError, BrokenPipeError, ValueError) as e:
            logging.info("client #%d reader error: %s", cid, e)
        finally:
            if buf:
                logging.warning("client #%d dropping %d bytes of incomplete trailing data", cid, len(buf))
        if not close_stdin:
            with self.client_lock:
                if self.client_conn is c:
                    self.client_conn = None
                    logging.info("client #%d disconnected reason=client_eof", cid)
            try:
                c.close()
            except OSError:
                pass
            return

        # Signal the shutdown watchdog to close stdin and kill the subprocess.
        self.shutdown_event.set()


def serve(cmd_args, work_dir, log_stdin=True, strip_env=(), shutdown_grace=_DEFAULT_SHUTDOWN_GRACE):
    """Start the relay server as a daemon, then attach as the first client.

    Architecture:
      Parent process → waits for socket → attach_client() (bridges stdio)
      Child process (daemon):
        1. Starts subprocess (claude/gemini) with piped stdin/stdout.
        2. reader_thread: subprocess stdout → output.jsonl + connected client.
        3. accept_thread: accepts client connections on Unix socket.
           - On connect: replays output.jsonl from offset, then forwards live.
           - client_reader: client stdin → subprocess stdin + optionally log.
        4. When subprocess exits:
           - reader_thread closes output file and disconnects client.
           - Socket and PID file are cleaned up.

    Args:
      log_stdin: When False, client_reader forwards stdin to the subprocess
        but does NOT write it to output.jsonl. This keeps the log clean for
        protocols like JSON-RPC where stdin contains handshake/request noise.
      strip_env: List of environment variable names to strip from the
        subprocess environment. Stripped values are emitted as a
        caic_stripped_env event after the first subprocess output.
      shutdown_grace: Seconds to wait after SIGINT before escalating to
        SIGTERM, then SIGKILL.

    Failure modes handled:
      - SSH drops: client disconnects, subprocess keeps running. Next
        attach reconnects from the offset where the client left off.
      - Subprocess crash: reader_thread exits, client sees EOF.
        Socket is cleaned up so IsRelayRunning returns false.
      - Graceful shutdown: client sends \\x00\\n sentinel → relay closes
        proc.stdin, sends SIGINT, escalates to SIGTERM/SIGKILL.
    """
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

    # Close inherited stdio FDs. The daemon communicates via the Unix socket
    # and subprocess pipes, not via its parent's stdio. Leaving them open
    # leaks the parent's pipe FDs, which can prevent the attach_client from
    # exiting cleanly on macOS.
    devnull = os.open(os.devnull, os.O_RDWR)
    os.dup2(devnull, 0)  # stdin → /dev/null
    os.dup2(devnull, 1)  # stdout → /dev/null
    # stderr is redirected to relay.log below.
    os.close(devnull)

    # Set up logging to relay.log. This replaces the old stderr redirect so
    # that key lifecycle events are always recorded for diagnostics.
    log_path = os.path.join(RELAY_DIR, "relay.log")
    logging.basicConfig(
        filename=log_path,
        filemode="w",
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
    )
    # Also capture stray stderr writes (e.g. from tracebacks).
    log_fd = os.open(log_path, os.O_WRONLY | os.O_APPEND)
    os.dup2(log_fd, 2)
    os.close(log_fd)

    # Write PID file.
    with open(PID_PATH, "w") as f:
        f.write(str(os.getpid()))

    logging.info("relay daemon started pid=%d cmd=%s cwd=%s", os.getpid(), cmd_args, work_dir)
    _start_time = time.monotonic()

    # Strip requested env vars from the subprocess so it authenticates via
    # OAuth. Stripped values are emitted as a caic_stripped_env event so the
    # backend can re-inject them after auth completes.
    env = os.environ.copy()
    pending_env = [k for k in strip_env if k in env]
    stripped_vars = {k: env.pop(k) for k in pending_env}
    if stripped_vars:
        logging.info("stripped env vars: %s", pending_env)

    proc = subprocess.Popen(
        cmd_args,
        cwd=work_dir,
        env=env,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
    )
    logging.info("subprocess started pid=%d", proc.pid)

    # Drain subprocess stderr to relay log so bridge diagnostics are visible.
    def _drain_stderr():
        for raw in proc.stderr:
            line = raw.decode("utf-8", errors="replace").rstrip()
            if line:
                logging.info("bridge: %s", line)

    threading.Thread(target=_drain_stderr, daemon=True).start()

    # Listen on Unix socket for client connections.
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(SOCK_PATH)
    srv.listen(1)

    output_file = open(OUTPUT_PATH, "ab", buffering=0)
    env_event = b""
    if stripped_vars:
        env_event = (json.dumps({"type": "caic_stripped_env", "variables": stripped_vars}) + "\n").encode()
    d = _Daemon(proc, output_file, work_dir, log_stdin, env_event)

    # reader_thread is the authoritative "done" signal: it reads stdout
    # until EOF (which implies the subprocess exited), flushes output.jsonl,
    # and closes the client socket so attach_client sees a clean EOF.
    # We join it instead of proc.wait() to avoid a race where the main
    # thread exits (killing daemon threads) before reader_thread has
    # delivered the last chunk to the client.
    reader_t = threading.Thread(target=d.reader_thread)
    threading.Thread(target=d.diff_watcher_thread, daemon=True).start()
    threading.Thread(target=d.accept_thread, args=(srv,), daemon=True).start()

    # Shutdown watchdog: waits for _client_reader to set shutdown_event,
    # then closes stdin + sends SIGINT, and escalates if the subprocess
    # doesn't exit (which makes reader_thread see stdout EOF).
    def _shutdown_watchdog():
        d.shutdown_event.wait()
        if not reader_t.is_alive():
            return  # Subprocess already exited naturally.
        logging.info("shutdown: closing stdin, sending SIGINT to pid=%d", proc.pid)
        try:
            proc.stdin.close()
        except OSError:
            pass
        try:
            proc.send_signal(signal.SIGINT)
        except OSError:
            pass
        reader_t.join(timeout=shutdown_grace)
        if not reader_t.is_alive():
            logging.info("shutdown: subprocess exited after SIGINT, proc_poll=%s", proc.poll())
            return
        logging.warning("shutdown: no exit after %ds, sending SIGTERM (proc_poll=%s)", shutdown_grace, proc.poll())
        try:
            proc.terminate()
        except OSError:
            pass
        reader_t.join(timeout=2)
        if not reader_t.is_alive():
            logging.info("shutdown: subprocess exited after SIGTERM, proc_poll=%s", proc.poll())
            return
        logging.warning("shutdown: subprocess did not exit after SIGTERM, sending SIGKILL (proc_poll=%s)", proc.poll())
        try:
            proc.kill()
        except OSError:
            pass

    watchdog_t = threading.Thread(target=_shutdown_watchdog)
    watchdog_t.start()
    reader_t.start()
    reader_t.join()
    # Unblock watchdog if subprocess exited naturally (no sentinel received).
    d.shutdown_event.set()
    watchdog_t.join()
    elapsed = time.monotonic() - _start_time
    logging.info("relay exiting code=%d elapsed=%.0fs", proc.returncode, elapsed)

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
    # The null byte sentinel for graceful shutdown is written by the Go
    # backend *before* closing stdin, so it arrives through the normal data
    # path. We must NOT inject a synthetic null byte on stdin EOF because
    # EOF also happens on SSH drops / backend restarts where the container
    # should keep running.
    try:
        while True:
            data = sys.stdin.buffer.read1(BUF_SIZE)
            if not data:
                break
            conn.sendall(data)
    except (OSError, BrokenPipeError, ValueError, KeyboardInterrupt):
        pass
    finally:
        # Half-close the socket write side so the relay daemon sees EOF on
        # client_reader, while keeping the read side open for relay_to_stdout
        # to drain the subprocess's final output (including the ResultMessage
        # emitted after stdin close).
        try:
            conn.shutdown(socket.SHUT_WR)
        except OSError:
            pass
        t.join(timeout=25)
        conn.close()


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
    raise TimeoutError("relay: timed out waiting for socket")


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
        return 0
    plans_dir = os.path.expanduser("~/.claude/plans")
    if not os.path.isdir(plans_dir):
        return 1
    files = [os.path.join(plans_dir, f) for f in os.listdir(plans_dir) if f.endswith(".md")]
    if not files:
        return 1
    latest = max(files, key=os.path.getmtime)
    with open(latest) as f:
        sys.stdout.write(f.read())
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(prog="relay.py")
    sub = parser.add_subparsers(dest="mode")

    sa = sub.add_parser("serve-attach")
    sa.add_argument("--dir", required=True, dest="work_dir")
    sa.add_argument("--no-log-stdin", action="store_true")
    sa.add_argument("--strip-env", action="append", default=[], metavar="KEY")
    sa.add_argument(
        "--shutdown-grace",
        type=int,
        default=_DEFAULT_SHUTDOWN_GRACE,
        metavar="SEC",
        help="seconds to wait after SIGINT before SIGTERM (default: %(default)s)",
    )
    sa.add_argument("cmd", nargs="+")

    at = sub.add_parser("attach")
    at.add_argument("--offset", type=int, default=0)

    rp = sub.add_parser("read-plan")
    rp.add_argument("path", nargs="?")

    args = parser.parse_args()
    if args.mode == "serve-attach":
        serve(
            args.cmd,
            args.work_dir,
            log_stdin=not args.no_log_stdin,
            strip_env=args.strip_env,
            shutdown_grace=args.shutdown_grace,
        )
    elif args.mode == "attach":
        attach_client(args.offset)
    elif args.mode == "read-plan":
        return read_plan(args.path)
    else:
        parser.print_help(sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
