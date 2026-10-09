#!/usr/bin/env python3
"""Tests for the maintained v2 relay: canonical framing, lifecycle, and MCP configuration."""

from __future__ import annotations

import argparse
import importlib.util
import json
import logging
import os
import re
import select
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
import traceback
from dataclasses import dataclass
from pathlib import Path
from types import ModuleType

RELAY_PY = Path(__file__).with_name("relay_v2.py")
FIXTURE_PATH = Path(__file__).parents[1] / "testdata" / "v2_agent_records.json"
_AGENT_RECORD = re.compile(rb'^\{"t":"agent","ts":(?:0|[1-9][0-9]*)\.[0-9]{3},"msg":.+\}\n$')


@dataclass(frozen=True)
class EncoderVector:
    name: str
    observed_unix_ns: str
    expected_timestamp: str
    native_bytes: str
    record_bytes: str


@dataclass(frozen=True)
class AgentRecordFixture:
    encoder_vectors: tuple[EncoderVector, ...]


class RecordingFile:
    """Unclosed binary output usable after a daemon reader test finishes."""

    def __init__(self) -> None:
        self.data = bytearray()
        self.closed = False

    def write(self, data: bytes) -> int:
        if self.closed:
            raise ValueError("write to closed file")
        self.data.extend(data)
        return len(data)

    def flush(self) -> None:
        if self.closed:
            raise ValueError("flush of closed file")

    def tell(self) -> int:
        return len(self.data)

    def close(self) -> None:
        self.closed = True


class PartialWriteFile(RecordingFile):
    """Binary output that accepts at most one short chunk per write."""

    def __init__(self, max_write: int) -> None:
        super().__init__()
        self.max_write = max_write
        self.write_calls = 0
        self.flushes = 0

    def write(self, data: bytes) -> int:
        self.write_calls += 1
        return super().write(data[: self.max_write])

    def flush(self) -> None:
        self.flushes += 1
        super().flush()


class InterruptedWriteFile(PartialWriteFile):
    """Binary output that stops making progress after one partial write."""

    def __init__(self, failure: int | None | OSError) -> None:
        super().__init__(1)
        self.failure = failure

    def write(self, data: bytes) -> int | None:
        if self.write_calls == 1:
            self.write_calls += 1
            if isinstance(self.failure, OSError):
                raise self.failure
            return self.failure
        return super().write(data)


class RecordingSocket:
    """Socket test double that records sends and supplies configured receives."""

    def __init__(self, received: tuple[bytes, ...] = ()) -> None:
        self.received = list(received)
        self.sent = bytearray()
        self.closed = False

    def recv(self, _size: int) -> bytes:
        if not self.received:
            return b""
        return self.received.pop(0)

    def sendall(self, data: bytes) -> None:
        if self.closed:
            raise OSError("socket closed")
        self.sent.extend(data)

    def close(self) -> None:
        self.closed = True


class PersistenceCheckingSocket(RecordingSocket):
    """Client that requires complete, flushed persistence before each send."""

    def __init__(self, output: PartialWriteFile, expected_persisted: bytes) -> None:
        super().__init__()
        self.output = output
        self.expected_persisted = expected_persisted

    def sendall(self, data: bytes) -> None:
        assert bytes(self.output.data) == self.expected_persisted
        assert self.output.flushes == 1
        super().sendall(data)


class RecordingStdin:
    def __init__(self) -> None:
        self.data = bytearray()
        self.flushes = 0
        self.closed = False

    def write(self, data: bytes) -> int:
        self.data.extend(data)
        return len(data)

    def flush(self) -> None:
        self.flushes += 1

    def close(self) -> None:
        self.closed = True


class ChunkedStdout:
    def __init__(self, chunks: tuple[bytes, ...]) -> None:
        self.chunks = list(chunks)

    def read1(self, _size: int) -> bytes:
        if not self.chunks:
            return b""
        return self.chunks.pop(0)


class FakeProc:
    def __init__(self, chunks: tuple[bytes, ...] = ()) -> None:
        self.stdout = ChunkedStdout(chunks)
        self.stdin = RecordingStdin()
        self.stderr = ()
        self.returncode = 0

    def wait(self, timeout: float | None = None) -> int:
        del timeout
        return self.returncode

    def poll(self) -> int:
        return self.returncode


class SequenceClock:
    def __init__(self, start: int = 1_700_000_000_000_000_000) -> None:
        self.current = start
        self.calls = 0
        self.lock = threading.Lock()

    def __call__(self) -> int:
        with self.lock:
            value = self.current
            self.current += 1_000_000
            self.calls += 1
            return value


def _load_relay() -> ModuleType:
    spec = importlib.util.spec_from_file_location("relay_v2", RELAY_PY)
    if spec is None or spec.loader is None:
        raise AssertionError(f"could not load {RELAY_PY}")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _load_fixture() -> AgentRecordFixture:
    with FIXTURE_PATH.open(encoding="utf-8") as fixture_file:
        raw = json.load(fixture_file)
    return AgentRecordFixture(
        encoder_vectors=tuple(EncoderVector(**vector) for vector in raw["encoder_vectors"]),
    )


def test_oom_adjusted_command() -> None:
    """The agent command raises its OOM score while preserving every argument."""
    relay = _load_relay()
    command = [sys.executable, "-c", "import json, sys; print(json.dumps(sys.argv[1:]))", "spaces stay intact"]
    with tempfile.NamedTemporaryFile() as score_file:
        score_file.write(b"200\n")
        score_file.flush()
        relay._OOM_SCORE_ADJ_PATH = score_file.name
        completed = subprocess.run(
            relay._oom_adjusted_command(command),
            capture_output=True,
            check=True,
            text=True,
        )
        assert json.loads(completed.stdout) == ["spaces stay intact"]
        score_file.seek(0)
        assert score_file.read() == b"300\n"

    assert relay._increment_oom_score_adj("-1000\n") == -900
    assert relay._increment_oom_score_adj("1000\n") == 1000
    try:
        relay._increment_oom_score_adj("invalid")
    except ValueError:
        pass
    else:
        raise AssertionError("invalid OOM score adjustment did not fail")

    relay._OOM_SCORE_ADJ_PATH = "/missing/oom_score_adj"
    assert relay._oom_adjusted_command(command) == command


def _decode_records(data: bytes) -> list[dict[str, object]]:
    return [json.loads(line) for line in data.splitlines()]


def _make_env(relay_dir: str) -> dict[str, str]:
    env = os.environ.copy()
    env["CAIC_RELAY_DIR"] = relay_dir
    return env


def _wait_for_socket(sock_path: str, timeout: float = 5) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if os.path.exists(sock_path):
            try:
                candidate = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
                candidate.connect(sock_path)
                candidate.close()
                return
            except OSError:
                pass
        time.sleep(0.05)
    raise TimeoutError("relay socket did not appear")


def _wait_for_daemon_exit(pid_path: str, timeout: float = 15) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not os.path.exists(pid_path):
            return
        try:
            with open(pid_path, encoding="utf-8") as pid_file:
                pid = int(pid_file.read().strip())
            os.kill(pid, 0)
        except OSError:
            return
        time.sleep(0.1)
    raise AssertionError(f"relay daemon did not exit within {timeout}s")


def _cleanup(relay_dir: str) -> None:
    pid_path = os.path.join(relay_dir, "pid")
    if os.path.exists(pid_path):
        try:
            with open(pid_path, encoding="utf-8") as pid_file:
                pid = int(pid_file.read().strip())
            os.kill(pid, 9)
        except (OSError, ValueError):
            pass
    shutil.rmtree(relay_dir, ignore_errors=True)


def test_harness_caic_mcp_integrations() -> None:
    """Each harness receives only its integration and shares teardown semantics."""
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    config_path = os.path.join(relay_dir, "caic-mcp.json")
    extension_path = os.path.join(relay_dir, "caic-mcp.ts")
    opencode_path = os.path.join(relay_dir, "opencode-config.json")
    env = _make_env(relay_dir)

    try:
        for harness in ("claude", "codex", "opencode", "pi"):
            env["OPENCODE_CONFIG_CONTENT"] = '{"model":"existing" /* user comment */, "mcp":{"user":{}},}'
            proc = subprocess.Popen(
                [
                    sys.executable,
                    str(RELAY_PY),
                    "serve-attach",
                    "--dir",
                    relay_dir,
                    "--harness",
                    harness,
                    "--caic-mcp",
                    "--",
                    "sh",
                    "-c",
                    "printf '%s' \"$OPENCODE_CONFIG_CONTENT\" > opencode-config.json; echo ready; cat",
                ],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=env,
            )
            try:
                assert proc.stdout is not None
                for line in proc.stdout:
                    if b"ready" in line:
                        break
                else:
                    raise AssertionError("harness did not start")
                assert os.path.exists(config_path) == (harness == "claude")
                assert os.path.exists(extension_path) == (harness == "pi")
                if harness == "claude":
                    config = json.loads(Path(config_path).read_text())
                    server = config["mcpServers"]["caic"]
                    assert server["command"] == "python3"
                    assert server["args"] == [str(RELAY_PY), "caic-mcp"]
                    assert os.stat(config_path).st_mode & 0o777 == 0o600
                if harness == "pi":
                    extension = Path(extension_path).read_text()
                    assert 'method: "tools/list"' in extension
                    assert "for (const tool of tools)" in extension
                    assert 'name: "task_create"' not in extension
                    assert "socketPath" in extension
                    assert os.stat(extension_path).st_mode & 0o777 == 0o600
                assert Path(opencode_path).read_text() == env["OPENCODE_CONFIG_CONTENT"]
                assert proc.stdin is not None
                proc.stdin.write(b"\x00\n")
                proc.stdin.flush()
                proc.stdin.close()
                proc.wait(timeout=15)
                deadline = time.monotonic() + 5
                owned_paths = (config_path, extension_path, os.path.join(relay_dir, "caic-mcp.sock"))
                while any(os.path.exists(path) for path in owned_paths):
                    assert time.monotonic() < deadline, "MCP teardown did not finish"
                    time.sleep(0.05)
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait(timeout=15)
    finally:
        _cleanup(relay_dir)


def test_harness_without_caic_mcp() -> None:
    """Harness identity alone starts the process without task-MCP integration."""
    with tempfile.TemporaryDirectory(prefix="caic-harness-test-") as root:
        for harness in ("antigravity", "claude", "codex", "opencode", "pi"):
            relay_dir = os.path.join(root, harness)
            result = subprocess.run(
                [
                    sys.executable,
                    str(RELAY_PY),
                    "serve-attach",
                    "--dir",
                    root,
                    "--harness",
                    harness,
                    "--",
                    sys.executable,
                    "-c",
                    "import json, os, sys; "
                    "print(json.dumps(dict(args=sys.argv[1:], files=os.listdir(os.environ['CAIC_RELAY_DIR']))))",
                ],
                input=b"",
                capture_output=True,
                env=_make_env(relay_dir),
                timeout=15,
            )
            # Empty stdin detaches the client like an SSH drop, possibly before
            # the agent prints, so read the durable log once the daemon exits.
            _wait_for_daemon_exit(os.path.join(relay_dir, "pid"))
            records = _decode_records(Path(relay_dir, "output.jsonl").read_bytes())
            messages = [record["msg"] for record in records if record["t"] == "agent"]
            assert len(messages) == 1, (result.stdout, result.stderr)
            assert messages[0]["args"] == []
            assert not any(name.startswith("caic-mcp") or name == "antigravity-mcp" for name in messages[0]["files"])
        result = subprocess.run(
            [sys.executable, str(RELAY_PY), "serve-attach", "--dir", root, "--caic-mcp", "--", "true"],
            capture_output=True,
            env=_make_env(os.path.join(root, "invalid")),
            timeout=5,
        )
        assert result.returncode == 2
        assert b"--caic-mcp requires --harness" in result.stderr


def _new_daemon(relay: ModuleType, *, chunks: tuple[bytes, ...] = (), log_stdin: bool = True):
    proc = FakeProc(chunks)
    output = RecordingFile()
    daemon = relay._Daemon(proc, output, ".", log_stdin, b"", ["fake-agent"])
    daemon.stderr_done.set()
    client = RecordingSocket()
    daemon.set_client(client, "test")
    return daemon, proc, output, client


def test_caic_mcp_bridge() -> None:
    """A local task-MCP request reaches the attached server and receives its reply."""
    relay = _load_relay()
    daemon, _proc, _output, client = _new_daemon(relay)
    server, local = socket.socketpair()
    try:
        request = {
            "id": "request-1",
            "method": "tools/call",
            "name": "task_create",
            "arguments": {"prompt": "write tests"},
        }
        local.sendall((json.dumps(request) + "\n").encode())
        thread = threading.Thread(target=daemon._handle_caic_mcp, args=(server,))
        thread.start()
        thread.join(timeout=1)
        assert not thread.is_alive()
        assert _decode_records(bytes(client.sent)) == [
            {
                "t": "mcp_request",
                "id": "request-1",
                "method": "tools/call",
                "name": "task_create",
                "arguments": {"prompt": "write tests"},
            }
        ]

        assert daemon.respond_caic_mcp({"id": "request-1", "result": {"content": []}})
        assert json.loads(relay._read_line(local)) == {"id": "request-1", "result": {"content": []}}
    finally:
        local.close()


def test_caic_mcp_request_isolation() -> None:
    """Request IDs cannot replace another client or cross relay lifetimes."""
    relay = _load_relay()
    first, _proc, _output, _client = _new_daemon(relay)
    second, _proc, _output, _client = _new_daemon(relay)
    raw = b'{"id":"same-id","method":"tools/call","name":"echo","arguments":{}}\n'
    local_first = RecordingSocket((raw,))
    local_second = RecordingSocket((raw,))
    first._handle_caic_mcp(local_first)
    duplicate = RecordingSocket((raw,))
    first._handle_caic_mcp(duplicate)
    assert duplicate.closed and "duplicate" in json.loads(duplicate.sent)["error"]
    assert not local_first.closed
    assert not second.respond_caic_mcp({"id": "same-id", "result": "wrong task"})
    second._handle_caic_mcp(local_second)
    assert first.respond_caic_mcp({"id": "same-id", "result": "first task"})
    assert second.respond_caic_mcp({"id": "same-id", "result": "second task"})
    assert json.loads(local_first.sent)["result"] == "first task"
    assert json.loads(local_second.sent)["result"] == "second task"
    pending = RecordingSocket((raw,))
    first._handle_caic_mcp(pending)
    first.close_caic_mcp_clients()
    assert pending.closed and not first.caic_mcp_clients
    assert not first.respond_caic_mcp({"id": "same-id", "result": "stale"})
    stopped = RecordingSocket((raw,))
    first._handle_caic_mcp(stopped)
    assert stopped.closed and "shutting down" in json.loads(stopped.sent)["error"]
    for value in ([], {}, {"id": "invalid", "method": "tools/call", "name": "", "arguments": {}}):
        bad = RecordingSocket(((json.dumps(value) + "\n").encode(),))
        second._handle_caic_mcp(bad)
        assert bad.closed and "error" in json.loads(bad.sent)
    assert not second.caic_mcp_clients


def test_caic_mcp_stdio_server() -> None:
    """The local stdio MCP server forwards normal tool calls through the relay socket."""
    relay = _load_relay()
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-test-")
    socket_path = os.path.join(relay_dir, "caic-mcp.sock")
    server = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    server.bind(socket_path)
    server.listen(1)
    received: list[dict[str, object]] = []

    def respond() -> None:
        for _ in range(2):
            conn, _ = server.accept()
            try:
                request = json.loads(relay._read_line(conn))
                received.append(request)
                if request["method"] == "tools/list":
                    result = {"tools": [{"name": "example_tool", "inputSchema": {"type": "object"}}]}
                else:
                    result = {
                        "content": [{"type": "text", "text": "Created child task: child-1"}],
                        "structuredContent": {"taskID": "child-1"},
                    }
                conn.sendall((json.dumps({"id": request["id"], "result": result}) + "\n").encode())
            finally:
                conn.close()

    thread = threading.Thread(target=respond)
    thread.start()
    proc = subprocess.Popen(
        [sys.executable, str(RELAY_PY), "caic-mcp"],
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        env=_make_env(relay_dir),
    )
    try:
        assert proc.stdin is not None
        assert proc.stdout is not None
        proc.stdin.write(json.dumps({"jsonrpc": "2.0", "id": 1, "method": "initialize"}) + "\n")
        proc.stdin.flush()
        initialized = json.loads(proc.stdout.readline())
        assert initialized["id"] == 1
        assert initialized["result"]["capabilities"] == {"tools": {}}

        proc.stdin.write(json.dumps({"jsonrpc": "2.0", "id": 2, "method": "tools/list"}) + "\n")
        proc.stdin.flush()
        listed = json.loads(proc.stdout.readline())
        assert listed == {
            "jsonrpc": "2.0",
            "id": 2,
            "result": {"tools": [{"name": "example_tool", "inputSchema": {"type": "object"}}]},
        }

        proc.stdin.write(
            json.dumps(
                {
                    "jsonrpc": "2.0",
                    "id": 3,
                    "method": "tools/call",
                    "params": {"name": "example_tool", "arguments": {"prompt": "write tests"}},
                }
            )
            + "\n"
        )
        proc.stdin.flush()
        result = json.loads(proc.stdout.readline())
        assert result == {
            "jsonrpc": "2.0",
            "id": 3,
            "result": {
                "content": [{"type": "text", "text": "Created child task: child-1"}],
                "structuredContent": {"taskID": "child-1"},
            },
        }
        thread.join(timeout=1)
        assert not thread.is_alive()
        assert len(received) == 2
        assert received[0]["method"] == "tools/list"
        assert received[1]["name"] == "example_tool"
        assert received[1]["arguments"] == {"prompt": "write tests"}
        assert received[1]["id"]
    finally:
        try:
            proc.kill()
        except OSError:
            pass
        proc.wait(timeout=5)
        server.close()
        _cleanup(relay_dir)


def test_shared_encoder_vectors() -> None:
    """The shared fixture alone supplies raw observations and expected bytes."""
    relay = _load_relay()
    fixture = _load_fixture()
    for vector in fixture.encoder_vectors:
        got = relay._encode_agent_record(vector.native_bytes.encode(), int(vector.observed_unix_ns))
        assert got == vector.record_bytes.encode(), vector.name
        assert _AGENT_RECORD.fullmatch(got), vector.name


def test_native_bytes_and_outer_shape() -> None:
    relay = _load_relay()
    native_values = (
        b'{"type":"native","nested":{"items":[1,true,{"escaped":"quote: \\""}]}}',
        b'[1, {"key" : [false, 2]}, true]',
        b'"string scalar"',
        b"-12.50e+2",
        b"true",
    )
    for native in native_values:
        padded = b" \t\r" + native + b"\t "
        record = relay._encode_agent_record(padded, 1_700_000_000_123_000_000)
        assert _AGENT_RECORD.fullmatch(record), record[:200]
        assert record.endswith(b',"msg":' + native + b"}\n"), record[-200:]
        assert b'"type":"native"' in record if b'"type":"native"' in native else b'"type"' not in record
        assert record.count(b'"t":"agent"') == 1
        assert record.find(b'"t"') < record.find(b'"ts"') < record.find(b'"msg"')


def test_invalid_native_values_use_bounded_diagnostics() -> None:
    relay = _load_relay()
    cases = (b"null", b"{not-json}", b"\xff", b" \t\r")
    for native in cases:
        record = relay._encode_agent_record(native, 1_700_000_000_000_000_000)
        decoded = json.loads(record)
        assert decoded["t"] == "agent"
        assert isinstance(decoded["msg"], str)
        assert decoded["msg"]
        assert b'"msg":null' not in record
        assert len(record) < relay._MAX_ENCODED_RECORD_LEN


def test_oversize_and_final_encoded_size_use_one_diagnostic() -> None:
    relay = _load_relay()
    old_limit = relay._MAX_ENCODED_RECORD_LEN
    old_preview = relay._DIAGNOSTIC_PREVIEW_BYTES
    try:
        relay._MAX_ENCODED_RECORD_LEN = 160
        relay._DIAGNOSTIC_PREVIEW_BYTES = 8
        # The native value fits this artificial limit, but its final envelope does not.
        native = b'"' + (b"x" * 130) + b'"'
        assert len(native) < relay._MAX_ENCODED_RECORD_LEN
        record = relay._encode_agent_record(native, 1_700_000_000_000_000_000)
        records = record.splitlines()
        assert len(records) == 1
        decoded = json.loads(record)
        assert isinstance(decoded["msg"], str)
        assert "oversized" in decoded["msg"]
        assert len(record) < relay._MAX_ENCODED_RECORD_LEN
        assert b"x" * 20 not in record

        daemon, _proc, output, client = _new_daemon(relay)
        try:
            daemon.publish_control("exit", {"error": "x" * 200}, to_client=True)
        except ValueError:
            pass
        else:
            raise AssertionError("emitted oversized control")
        assert output.data == b""
        assert client.sent == b""
    finally:
        relay._MAX_ENCODED_RECORD_LEN = old_limit
        relay._DIAGNOSTIC_PREVIEW_BYTES = old_preview


def test_timestamp_failures_precede_emission() -> None:
    relay = _load_relay()
    invalid_observations = (
        0,
        -1,
        1,  # Positive raw time that would round to forbidden 0.000.
        (relay._MAX_UNIX_SECONDS + 1) * 1_000_000_000,
        1 << 200,
    )
    for observed_ns in invalid_observations:
        try:
            relay._format_timestamp(observed_ns)
        except ValueError:
            pass
        else:
            raise AssertionError(f"accepted invalid observation {observed_ns}")

        daemon, _proc, output, client = _new_daemon(relay)
        try:
            daemon.publish_agent(b"true", observed_unix_ns=observed_ns, to_client=True)
        except ValueError:
            pass
        else:
            raise AssertionError(f"emitted invalid observation {observed_ns}")
        assert output.data == b""
        assert client.sent == b""

    valid = relay._encode_agent_record(b"true", 500_000)
    assert _AGENT_RECORD.fullmatch(valid)


def test_publish_records_persists_partial_writes_before_client_send() -> None:
    relay = _load_relay()
    records = (
        relay._encode_agent_record(b'{"partial":true}', 1_700_000_000_000_000_000),
        relay._encode_control("diff_stat", {"diff_stat": [], "ts": 1}),
    )
    expected = b"".join(records)
    output = PartialWriteFile(7)
    daemon = relay._Daemon(FakeProc(), output, ".", True, b"", ["fake-agent"])
    client = PersistenceCheckingSocket(output, expected)
    daemon.set_client(client, "partial-write-test")

    daemon.publish_records(*records, to_client=True)

    assert output.write_calls > len(records)
    assert bytes(output.data) == expected
    assert bytes(client.sent) == expected
    assert output.flushes == 1

    for failure in (0, None, OSError("write failed")):
        output = InterruptedWriteFile(failure)
        daemon = relay._Daemon(FakeProc(), output, ".", True, b"", ["fake-agent"])
        client = RecordingSocket()
        daemon.set_client(client, "write-failure-test")
        try:
            daemon.publish_records(records[0], to_client=True)
        except OSError as error:
            if isinstance(failure, OSError):
                assert error is failure
        else:
            raise AssertionError(f"accepted output write failure {failure!r}")
        assert bytes(output.data) == records[0][:1]
        assert client.sent == b""
        assert output.flushes == 0


def test_stdout_chunk_carry_blank_and_eof_flush() -> None:
    relay = _load_relay()
    daemon, _proc, output, client = _new_daemon(
        relay,
        chunks=(b' {"first":', b"1} \n\ntr", b'ue\n{"partial":2}'),
    )
    clock = SequenceClock()
    relay._observe_unix_ns = clock
    daemon.reader_thread()

    records = _decode_records(bytes(output.data))
    agents = [record for record in records if record["t"] == "agent"]
    assert [agents[0]["msg"], agents[2]["msg"], agents[3]["msg"]] == [
        {"first": 1},
        True,
        {"partial": 2},
    ]
    assert isinstance(agents[1]["msg"], str), agents[1]
    assert records[-1]["t"] == "exit"
    assert bytes(client.sent) == bytes(output.data)
    assert clock.calls == 4


def test_logged_stdin_is_file_only_and_partial_eof_is_dropped() -> None:
    relay = _load_relay()
    daemon, proc, output, client = _new_daemon(relay, log_stdin=True)
    clock = SequenceClock()
    relay._observe_unix_ns = clock
    incoming = RecordingSocket((b' {"one":', b"1} \ntrue\npartial", b""))
    daemon.set_client(incoming, "stdin-test")
    daemon._client_reader(incoming, 1)

    assert bytes(proc.stdin.data) == b' {"one":1} \ntrue\n'
    records = _decode_records(bytes(output.data))
    assert [record["msg"] for record in records] == [{"one": 1}, True]
    assert incoming.sent == b""
    assert client.sent == b""
    assert clock.calls == 2


def test_unlogged_stdin_is_forwarded_without_persistence() -> None:
    relay = _load_relay()
    daemon, proc, output, _client = _new_daemon(relay, log_stdin=False)
    incoming = RecordingSocket((b'{"request":1}\n', b""))
    daemon.set_client(incoming, "stdin-test")
    daemon._client_reader(incoming, 1)

    assert bytes(proc.stdin.data) == b'{"request":1}\n'
    assert output.data == b""
    assert incoming.sent == b""


def test_concurrent_controls_keep_destination_order_and_v2_tokens() -> None:
    relay = _load_relay()
    daemon, _proc, output, client = _new_daemon(relay)
    start = threading.Barrier(17)

    def emit(index: int) -> None:
        start.wait()
        daemon.publish_control("diff_stat", {"diff_stat": [{"path": str(index)}], "ts": index}, to_client=True)

    threads = [threading.Thread(target=emit, args=(index,)) for index in range(16)]
    for thread in threads:
        thread.start()
    start.wait()
    for thread in threads:
        thread.join()

    assert bytes(output.data) == bytes(client.sent)
    records = _decode_records(bytes(output.data))
    assert len(records) == 16
    assert all(record["t"] == "diff_stat" for record in records)
    assert all("type" not in record for record in records)

    invalid_controls = (
        ("caic_diff_stat", {"diff_stat": []}),
        ("diff_stat", {"t": "diff_stat"}),
        ("diff_stat", {"type": "caic_diff_stat"}),
    )
    for token, fields in invalid_controls:
        try:
            relay._encode_control(token, fields)
        except ValueError:
            pass
        else:
            raise AssertionError(f"accepted forbidden control {token!r} with {fields!r}")


def test_real_relay_output_superset_no_stdin_echo_and_attach_offset() -> None:
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-v2-test-")
    sock_path = os.path.join(relay_dir, "relay.sock")
    output_path = os.path.join(relay_dir, "output.jsonl")
    pid_path = os.path.join(relay_dir, "pid")
    proc: subprocess.Popen[bytes] | None = None
    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                str(RELAY_PY),
                "serve-attach",
                "--dir",
                relay_dir,
                "--",
                sys.executable,
                "-c",
                (
                    "import sys\n"
                    "for line in sys.stdin:\n"
                    "    print(line.replace('stdin', 'stdout'), end='', flush=True)\n"
                ),
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=_make_env(relay_dir),
        )
        assert proc.stdin is not None
        assert proc.stdout is not None
        generation = proc.stdout.readline()
        generation_record = json.loads(generation)
        assert generation_record["t"] == "relay_generation"
        assert generation_record["generation"]
        proc.stdin.write(b'{"source":"stdin"}\n')
        proc.stdin.flush()
        live = proc.stdout.readline()
        assert _AGENT_RECORD.fullmatch(live), live

        deadline = time.monotonic() + 5
        lines: list[bytes] = []
        while time.monotonic() < deadline:
            try:
                with open(output_path, "rb") as output_file:
                    lines = output_file.readlines()
                if len(lines) >= 3:
                    break
            except FileNotFoundError:
                pass
            time.sleep(0.05)
        assert len(lines) == 3, lines
        assert lines[0] == generation
        assert live == lines[2]
        assert lines[0] != live
        assert json.loads(lines[1])["msg"] == {"source": "stdin"}
        assert json.loads(lines[2])["msg"] == {"source": "stdout"}

        # Plain EOF preserves the daemon and agent, matching v1 SSH-drop semantics.
        proc.stdin.close()
        proc.wait(timeout=10)
        _wait_for_socket(sock_path, timeout=3)

        full_replay = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        full_replay.settimeout(5)
        full_replay.connect(sock_path)
        full_replay.sendall(b'{"offset": 0}\n')
        replay = b""
        while len(replay) < sum(map(len, lines)):
            chunk = full_replay.recv(65536)
            assert chunk, (replay, lines)
            replay += chunk
        assert replay.startswith(b"".join(lines)), (replay, lines)
        full_replay.close()

        conn = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        conn.settimeout(5)
        conn.connect(sock_path)
        conn.sendall(json.dumps({"offset": len(lines[0]) + len(lines[1])}).encode() + b"\n")
        replay = conn.recv(65536)
        assert replay.startswith(lines[2]), (replay, lines)
        conn.sendall(b"\x00\n")
        conn.close()
        _wait_for_daemon_exit(pid_path, timeout=10)
    finally:
        if proc is not None:
            try:
                proc.kill()
            except OSError:
                pass
        _cleanup(relay_dir)


def test_exit_and_stripped_environment_controls() -> None:
    relay_dir = tempfile.mkdtemp(prefix="caic-relay-v2-test-")
    output_path = os.path.join(relay_dir, "output.jsonl")
    release_path = os.path.join(relay_dir, "release-child")
    env = _make_env(relay_dir)
    env["CAIC_RELAY_TEST_SECRET"] = "not-persisted"
    proc: subprocess.Popen[bytes] | None = None
    try:
        proc = subprocess.Popen(
            [
                sys.executable,
                str(RELAY_PY),
                "serve-attach",
                "--dir",
                relay_dir,
                "--strip-env",
                "CAIC_RELAY_TEST_SECRET",
                "--",
                sys.executable,
                "-c",
                (
                    "import os, sys, time\n"
                    "deadline = time.monotonic() + 10\n"
                    "while not os.path.exists(sys.argv[1]):\n"
                    "    if time.monotonic() >= deadline: raise SystemExit('child was not released')\n"
                    "    time.sleep(0.01)\n"
                    "print('{\"ready\":true}')\n"
                    "raise SystemExit(2 if 'CAIC_RELAY_TEST_SECRET' in os.environ else 0)\n"
                ),
                release_path,
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        # Keep stdin open until the daemon closes the attach socket at
        # subprocess EOF. Closing stdin first exercises the plain-EOF
        # (SSH-drop) path, where the daemon tears down the live socket before
        # the subprocess output arrives, so stdout can be an early truncation
        # of output.jsonl instead of the full stream.
        assert proc.stdin is not None
        assert proc.stdout is not None
        # Force the live-attach path: a fast child can otherwise exit before
        # attachment and exercise only the durable-log fallback.
        deadline = time.monotonic() + 5
        stdout = b""
        while not stdout.endswith(b"\n"):
            remaining = deadline - time.monotonic()
            assert remaining > 0 and select.select([proc.stdout], [], [], remaining)[0], "relay did not attach"
            chunk = os.read(proc.stdout.fileno(), 1)
            assert chunk, "relay closed stdout before attachment"
            stdout += chunk
        assert json.loads(stdout)["t"] == "relay_generation", stdout
        Path(release_path).touch()
        deadline = time.monotonic() + 5
        while True:
            remaining = deadline - time.monotonic()
            assert remaining > 0 and select.select([proc.stdout], [], [], remaining)[0], (
                "relay did not close stdout after child exit while stdin remained open"
            )
            chunk = os.read(proc.stdout.fileno(), 65536)
            if not chunk:
                break
            stdout += chunk
        proc.stdin.close()
        proc.wait(timeout=10)
        if proc.stderr is not None:
            proc.stderr.read()
        with open(output_path, "rb") as output_file:
            persisted = output_file.read()
        assert stdout == persisted
        records = _decode_records(persisted)
        assert [record["t"] for record in records] == ["relay_generation", "agent", "stripped_env", "exit"]
        assert records[0]["generation"]
        assert records[2]["variables"] == {"CAIC_RELAY_TEST_SECRET": ""}
        assert records[3]["exit_code"] == 0
        assert all("type" not in record for record in records)
        assert b"not-persisted" not in persisted
    finally:
        if proc is not None:
            try:
                proc.kill()
            except OSError:
                pass
        _cleanup(relay_dir)


def test_antigravity_mcp_lifecycle() -> None:
    """Auxiliary plugins preserve HOME/worktrees and are removed on exit/retry."""
    with tempfile.TemporaryDirectory(prefix="caic-agy-mcp-test-") as root:
        root = os.path.realpath(root)
        relay_dir = os.path.join(root, "relay")
        work_dir = os.path.join(root, "work")
        home = os.path.join(root, "home")
        os.makedirs(work_dir)
        os.makedirs(os.path.join(home, ".gemini", "config"))
        settings = Path(home, ".gemini", "config", "mcp_config.json")
        settings.write_text("user settings must survive", encoding="utf-8")
        env = {**_make_env(relay_dir), "HOME": home}
        auxiliary = Path(relay_dir, "antigravity-mcp")
        names = []
        for fail in (False, True, False):
            command = (
                ["caic-test-missing-executable"]
                if fail
                else [
                    sys.executable,
                    "-u",
                    "-c",
                    "import json, os, sys; "
                    "data = dict(home=os.environ['HOME'], cwd=os.getcwd(), args=sys.argv[1:]); "
                    "print(json.dumps(data), flush=True); sys.stdin.read()",
                ]
            )
            proc = subprocess.Popen(
                [
                    sys.executable,
                    str(RELAY_PY),
                    "serve-attach",
                    "--dir",
                    work_dir,
                    "--harness",
                    "antigravity",
                    "--caic-mcp",
                    "--",
                    *command,
                ],
                stdin=subprocess.PIPE,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                env=env,
            )
            try:
                if fail:
                    out, err = proc.communicate(timeout=10)
                    records = _decode_records(out)
                    exits = [record for record in records if record["t"] == "exit"]
                    assert exits and exits[0]["exit_code"] != 0, (out, err)
                else:
                    assert proc.stdout is not None
                    while True:
                        record = json.loads(proc.stdout.readline())
                        if record["t"] == "agent":
                            break
                    assert record["msg"] == {"home": home, "cwd": work_dir, "args": ["--add-dir", str(auxiliary)]}
                    plugins = list((auxiliary / ".agents" / "plugins").iterdir())
                    assert len(plugins) == 1
                    plugin = plugins[0]
                    names.append(plugin.name)
                    assert json.loads((plugin / "plugin.json").read_text())["name"] == plugin.name
                    config = json.loads((plugin / "mcp_config.json").read_text())
                    assert config["mcpServers"]["caic"] == {
                        "command": "python3",
                        "args": [str(RELAY_PY), "caic-mcp"],
                        "env": {"CAIC_RELAY_DIR": relay_dir},
                    }
                    assert auxiliary.stat().st_mode & 0o777 == 0o700
                    assert (plugin / "mcp_config.json").stat().st_mode & 0o777 == 0o600
                    assert not list(Path(work_dir).iterdir())
                    assert settings.read_text() == "user settings must survive"
                    proc.communicate(b"\x00\n", timeout=15)
                deadline = time.monotonic() + 5
                while auxiliary.exists() or os.path.exists(os.path.join(relay_dir, "caic-mcp.sock")):
                    assert time.monotonic() < deadline, "task MCP config/access survived teardown"
                    time.sleep(0.02)
                assert settings.read_text() == "user settings must survive"
            finally:
                if proc.poll() is None:
                    proc.kill()
                    proc.wait(timeout=5)
        assert len(set(names)) == 2, "retry reused a shared schema-cache namespace"


def test_caic_mcp_stdio_errors() -> None:
    """Unknown requests receive protocol errors; bad JSON does not kill retries."""
    messages = [
        "[]",
        "{",
        '{"jsonrpc":"2.0","id":7,"method":"server/discover"}',
        '{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"x","arguments":[]}}',
        '{"jsonrpc":"2.0","method":"unknown-notification"}',
        '{"jsonrpc":"2.0","id":9,"method":"initialize"}',
    ]
    proc = subprocess.run(
        [sys.executable, str(RELAY_PY), "caic-mcp"],
        input="\n".join(messages) + "\n",
        text=True,
        capture_output=True,
        timeout=5,
    )
    assert proc.returncode == 0, proc.stderr
    responses = [json.loads(line) for line in proc.stdout.splitlines()]
    assert len(responses) == 5, responses
    assert responses[0]["id"] is None and responses[0]["error"]["code"] == -32602
    assert responses[1]["id"] is None and responses[1]["error"]["code"] == -32602
    assert responses[2]["id"] == 7 and responses[2]["error"]["code"] == -32601
    assert responses[3]["id"] == 8 and responses[3]["error"]["code"] == -32602
    assert responses[4]["id"] == 9 and "result" in responses[4]


def test_parse_numstat() -> None:
    relay = _load_relay()
    result = relay._parse_numstat("10\t3\tsrc/main.go\n-\t-\timage.png\n")
    assert result == [
        {"path": "src/main.go", "added": 10, "deleted": 3},
        {"path": "image.png", "added": 0, "deleted": 0, "binary": True},
    ]

    # Binary sizes come from the appended --stat block and match by position.
    result = relay._parse_numstat(
        "10\t3\tsrc/main.go\n-\t-\timage.png\n"
        " src/main.go | 10 +--\n image.png  | Bin 0 -> 2048 bytes\n 2 files changed\n"
    )
    assert result == [
        {"path": "src/main.go", "added": 10, "deleted": 3},
        {"path": "image.png", "added": 0, "deleted": 0, "binary": True, "oldSize": 0, "newSize": 2048},
    ]


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the v2 relay unit tests.")
    parser.add_argument(
        "-v",
        "--verbose",
        action="store_true",
        help="print one progress line per test",
    )
    args = parser.parse_args()
    if not args.verbose:
        # The relay logs expected warnings (for example, dropping a partial
        # trailing record); silence them unless progress is requested.
        logging.disable(logging.ERROR)
    tests = (
        test_shared_encoder_vectors,
        test_native_bytes_and_outer_shape,
        test_invalid_native_values_use_bounded_diagnostics,
        test_oversize_and_final_encoded_size_use_one_diagnostic,
        test_timestamp_failures_precede_emission,
        test_publish_records_persists_partial_writes_before_client_send,
        test_stdout_chunk_carry_blank_and_eof_flush,
        test_logged_stdin_is_file_only_and_partial_eof_is_dropped,
        test_unlogged_stdin_is_forwarded_without_persistence,
        test_concurrent_controls_keep_destination_order_and_v2_tokens,
        test_real_relay_output_superset_no_stdin_echo_and_attach_offset,
        test_exit_and_stripped_environment_controls,
        test_harness_caic_mcp_integrations,
        test_harness_without_caic_mcp,
        test_oom_adjusted_command,
        test_caic_mcp_bridge,
        test_caic_mcp_request_isolation,
        test_caic_mcp_stdio_server,
        test_caic_mcp_stdio_errors,
        test_antigravity_mcp_lifecycle,
        test_parse_numstat,
    )
    failed: list[str] = []
    for test in tests:
        if args.verbose:
            print(f"{test.__name__}...", end=" ", flush=True)
        try:
            test()
        except Exception:
            if args.verbose:
                print("FAIL")
            failed.append(test.__name__)
            print(f"FAILED: {test.__name__}", file=sys.stderr)
            traceback.print_exc()
    if failed:
        print(f"\n{len(failed)} of {len(tests)} tests FAILED: {', '.join(failed)}", file=sys.stderr)
        return 1
    if args.verbose:
        print(f"\nAll {len(tests)} tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
