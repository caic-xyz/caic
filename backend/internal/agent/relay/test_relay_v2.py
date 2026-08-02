#!/usr/bin/env python3
"""Tests for the v2-only relay's canonical framing and v1 lifecycle semantics."""

from __future__ import annotations

import importlib.util
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import tempfile
import threading
import time
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


def _new_daemon(relay: ModuleType, *, chunks: tuple[bytes, ...] = (), log_stdin: bool = True):
    proc = FakeProc(chunks)
    output = RecordingFile()
    daemon = relay._Daemon(proc, output, ".", log_stdin, b"", ["fake-agent"])
    daemon.stderr_done.set()
    client = RecordingSocket()
    daemon.set_client(client, "test")
    return daemon, proc, output, client


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
                if len(lines) >= 2:
                    break
            except FileNotFoundError:
                pass
            time.sleep(0.05)
        assert len(lines) == 2, lines
        assert live == lines[1]
        assert lines[0] != live
        assert json.loads(lines[0])["msg"] == {"source": "stdin"}
        assert json.loads(lines[1])["msg"] == {"source": "stdout"}

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
        conn.sendall(json.dumps({"offset": len(lines[0])}).encode() + b"\n")
        replay = conn.recv(65536)
        assert replay.startswith(lines[1]), (replay, lines)
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
                    "import os; print('{\"ready\":true}'); "
                    "raise SystemExit(2 if 'CAIC_RELAY_TEST_SECRET' in os.environ else 0)"
                ),
            ],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        stdout, _stderr = proc.communicate(timeout=10)
        with open(output_path, "rb") as output_file:
            persisted = output_file.read()
        assert stdout == persisted
        records = _decode_records(persisted)
        assert [record["t"] for record in records] == ["agent", "stripped_env", "exit"]
        assert records[1]["variables"] == {"CAIC_RELAY_TEST_SECRET": ""}
        assert records[2]["exit_code"] == 0
        assert all("type" not in record for record in records)
        assert b"not-persisted" not in persisted
    finally:
        if proc is not None:
            try:
                proc.kill()
            except OSError:
                pass
        _cleanup(relay_dir)


def test_parse_numstat() -> None:
    relay = _load_relay()
    result = relay._parse_numstat("10\t3\tsrc/main.go\n-\t-\timage.png\n")
    assert result == [
        {"path": "src/main.go", "added": 10, "deleted": 3},
        {"path": "image.png", "added": 0, "deleted": 0, "binary": True},
    ]


def main() -> int:
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
        test_parse_numstat,
    )
    failed: list[str] = []
    for test in tests:
        print(f"{test.__name__}...", end=" ", flush=True)
        try:
            test()
            print("OK")
        except Exception as error:
            print(f"FAIL: {error}")
            failed.append(test.__name__)
    if failed:
        print(f"\n{len(failed)} FAILED: {', '.join(failed)}")
        return 1
    print(f"\nAll {len(tests)} tests passed.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
