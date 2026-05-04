#!/usr/bin/env python3
"""Analyze a Go runtime/trace binary file for performance issues.

Usage:
  python3 scripts/analyze_trace.py trace.out              # full report
  python3 scripts/analyze_trace.py --gc trace.out          # GC timeline only
  python3 scripts/analyze_trace.py --goroutines trace.out  # goroutine timeline only

Uses go tool trace -http to serve the trace, then fetches the jsontrace JSON.
Requires: go 1.22+
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import socket
import subprocess
import sys
import time
import urllib.request
from collections import Counter
from dataclasses import dataclass, field

# ---------------------------------------------------------------------------
# Trace server
# ---------------------------------------------------------------------------


def _find_free_port() -> int:
    """Find a free TCP port on localhost."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _start_trace_server(trace_path: str, port: int = 0) -> tuple[subprocess.Popen, int]:
    """Start go tool trace -http, return (process, port)."""
    if port == 0:
        port = _find_free_port()

    proc = subprocess.Popen(
        ["go", "tool", "trace", "-http", f"127.0.0.1:{port}", trace_path],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.PIPE,
        text=True,
    )

    deadline = time.time() + 30
    output_lines: list[str] = []
    while time.time() < deadline:
        line = proc.stderr.readline()  # type: ignore[union-attr]
        if line:
            output_lines.append(line)
            if "listening on" in line:
                time.sleep(0.3)  # let it finish binding
                return proc, port
        if proc.poll() is not None:
            break
        time.sleep(0.1)

    proc.kill()
    raise RuntimeError(f"go tool trace did not start within 30s:\n{''.join(output_lines)}")


def _fetch_trace_json(port: int) -> dict[str, object]:
    """Fetch jsontrace JSON from the trace HTTP server."""
    url = f"http://127.0.0.1:{port}/jsontrace?trace="
    with urllib.request.urlopen(url, timeout=60) as resp:
        return json.loads(resp.read())  # type: ignore[no-any-return]


def _parse_time_us(ts_us: float) -> float:
    """Convert microsecond timestamp to seconds."""
    return ts_us / 1e6


# ---------------------------------------------------------------------------
# Trace event data model
# ---------------------------------------------------------------------------


@dataclass
class MarkEvent:
    time_s: float
    duration_ms: float


@dataclass
class GoroutineSlice:
    duration_ms: float
    time_s: float
    name: str
    goroutine_id: int


@dataclass
class UserLogEvent:
    time_s: float
    name: str


@dataclass
class ExecSlice:
    duration_ms: float
    time_s: float
    goroutine_id: int


@dataclass
class GCStats:
    num_marks: int
    num_stw: int
    total_mark_ms: float
    total_stw_ms: float
    max_mark_ms: float
    marks: list[MarkEvent] = field(default_factory=list)


@dataclass
class GoroutineStats:
    max_concurrent: int
    timeline_5s: list[tuple[int, int]]  # (bucket_start_s, count)
    top_slices: list[GoroutineSlice] = field(default_factory=list)


@dataclass
class HTTPStats:
    first_request_s: float | None
    total_requests: int
    longest_ms: float


@dataclass
class ExecStats:
    count: int
    total_ms: float
    max_ms: float
    top: list[ExecSlice] = field(default_factory=list)


@dataclass
class Verdict:
    """Diagnostic findings that warrant attention."""

    level: str  # "ok", "warn", "bad"
    message: str


# ---------------------------------------------------------------------------
# Analysis
# ---------------------------------------------------------------------------


def _raw_events(data: dict[str, object]) -> list[dict[str, object]]:
    events = data.get("traceEvents")
    if not isinstance(events, list):
        return []
    return events  # type: ignore[return-type]


def analyze_gc(events: list[dict[str, object]]) -> GCStats:
    marks: list[MarkEvent] = []
    stw_pauses: list[MarkEvent] = []
    for e in events:
        name = str(e.get("name", ""))
        dur = float(e.get("dur", 0))
        ts = float(e.get("ts", 0))
        if "concurrent mark" in name and dur > 0:
            marks.append(MarkEvent(time_s=_parse_time_us(ts), duration_ms=dur / 1000))
        if "STW" in name or "stop-the-world" in name:
            stw_pauses.append(MarkEvent(time_s=_parse_time_us(ts), duration_ms=dur / 1000))

    marks.sort(key=lambda m: m.time_s)
    total_mark = sum(m.duration_ms for m in marks)
    total_stw = sum(m.duration_ms for m in stw_pauses)
    max_mark = max((m.duration_ms for m in marks), default=0)

    return GCStats(
        num_marks=len(marks),
        num_stw=len(stw_pauses),
        total_mark_ms=round(total_mark, 2),
        total_stw_ms=round(total_stw, 2),
        max_mark_ms=round(max_mark, 2),
        marks=marks,
    )


def analyze_goroutines(events: list[dict[str, object]]) -> GoroutineStats:
    timeline: list[tuple[float, int]] = []
    for e in events:
        if e.get("name") == "Goroutines":
            args = e.get("args", {})
            if isinstance(args, dict):
                running = int(args.get("Running", 0))
                timeline.append((float(e["ts"]), running))

    # Top slices by duration (non-GC, >1ms).
    slices: list[GoroutineSlice] = []
    for e in events:
        dur = float(e.get("dur", 0))
        name = str(e.get("name", ""))
        if dur > 1_000_000 and "GC" not in name and "gcBg" not in name:
            slices.append(
                GoroutineSlice(
                    duration_ms=round(dur / 1e6, 2),
                    time_s=round(_parse_time_us(float(e.get("ts", 0))), 3),
                    name=name,
                    goroutine_id=int(e.get("tid", 0)),
                )
            )
    slices.sort(key=lambda s: -s.duration_ms)

    # Bucket by 5s intervals.
    buckets: Counter[int] = Counter()
    for ts, count in timeline:
        b = int(ts / 1e6 / 5)
        buckets[b] = max(buckets.get(b, 0), count)

    return GoroutineStats(
        max_concurrent=max((c for _, c in timeline), default=0),
        timeline_5s=[(b * 5, c) for b, c in sorted(buckets.items())],
        top_slices=slices[:20],
    )


def analyze_user_events(events: list[dict[str, object]]) -> list[UserLogEvent]:
    logs: list[UserLogEvent] = []
    for e in events:
        if e.get("cat") == "user event":
            logs.append(
                UserLogEvent(
                    time_s=round(_parse_time_us(float(e.get("ts", 0))), 3),
                    name=str(e.get("name", "")),
                )
            )
    return logs


def analyze_http(events: list[dict[str, object]]) -> HTTPStats:
    requests: list[tuple[float, float]] = []
    for e in events:
        name = str(e.get("name", ""))
        if "conn" in name and "serve" in name:
            requests.append((float(e.get("ts", 0)), float(e.get("dur", 0))))
    requests.sort(key=lambda x: x[0])
    return HTTPStats(
        first_request_s=round(_parse_time_us(requests[0][0]), 3) if requests else None,
        total_requests=len(requests),
        longest_ms=round(max(d for _, d in requests) / 1000, 2) if requests else 0,
    )


def analyze_exec(events: list[dict[str, object]]) -> ExecStats:
    exec_events: list[ExecSlice] = []
    for e in events:
        name = str(e.get("name", ""))
        dur = float(e.get("dur", 0))
        if "exec" in name and dur > 0:
            exec_events.append(
                ExecSlice(
                    duration_ms=round(dur / 1000, 2),
                    time_s=round(_parse_time_us(float(e.get("ts", 0))), 3),
                    goroutine_id=int(e.get("tid", 0)),
                )
            )
    exec_events.sort(key=lambda x: -x.duration_ms)
    total = sum(e.duration_ms for e in exec_events)
    return ExecStats(
        count=len(exec_events),
        total_ms=round(total, 2),
        max_ms=exec_events[0].duration_ms if exec_events else 0,
        top=exec_events[:10],
    )


# ---------------------------------------------------------------------------
# Heuristics — flag issues
# ---------------------------------------------------------------------------


def verdicts(
    duration_s: float,
    gc: GCStats,
    gr: GoroutineStats,
    http: HTTPStats,
    exec_stats: ExecStats,
    user_events: list[UserLogEvent],
) -> list[Verdict]:
    out: list[Verdict] = []

    # 1. GC mark escalation near container operations.
    # Only flag escalation chains starting within 10s of a container
    # adoption event. Ignore initial heap warmup in the first second.
    adoption_times = [e.time_s for e in user_events if "container" in e.name]

    if adoption_times and len(gc.marks) >= 3:
        for i in range(len(gc.marks) - 1):
            prev = gc.marks[i]
            cur = gc.marks[i + 1]
            # Only flag if the first mark in the pair is near a container event.
            near = any(abs(prev.time_s - a) < 10.0 for a in adoption_times)
            if (
                near
                and cur.time_s - prev.time_s < 2.0
                and prev.duration_ms > 5
                and cur.duration_ms > prev.duration_ms * 2
            ):
                out.append(
                    Verdict(
                        "bad",
                        f"GC mark escalation: {prev.duration_ms:.0f}ms → "
                        f"{cur.duration_ms:.0f}ms within "
                        f"{cur.time_s - prev.time_s:.1f}s "
                        f"(@ {prev.time_s:.1f}s). Check for unbounded "
                        f"allocation in container adoption loop.",
                    )
                )
                break

    # 2. Single very large GC mark.
    if gc.max_mark_ms > 200:
        out.append(
            Verdict(
                "bad",
                f"Single GC mark of {gc.max_mark_ms:.0f}ms — likely blocking all progress.",
            )
        )
    elif gc.max_mark_ms > 50:
        out.append(
            Verdict(
                "warn",
                f"GC mark of {gc.max_mark_ms:.0f}ms — consider GOGC tuning or allocation review.",
            )
        )

    # 3. Startup latency: time to first HTTP.
    if http.first_request_s is not None:
        n_containers = len(user_events) - 1  # subtract the canary
        if http.first_request_s > 60:
            out.append(
                Verdict(
                    "warn",
                    f"Startup took {http.first_request_s:.1f}s to first HTTP "
                    f"({n_containers} containers adopted). "
                    f"{(http.first_request_s - 42):.1f}s elapsed after "
                    f"container discovery.",
                )
            )

    # 4. Idle gaps: goroutine count drops to ≤2 for >10s before HTTP.
    idle_start: float | None = None
    for bucket_s, count in gr.timeline_5s:
        if http.first_request_s is not None and bucket_s >= http.first_request_s:
            break
        if count <= 2:
            if idle_start is None:
                idle_start = float(bucket_s)
        else:
            if idle_start is not None and float(bucket_s) - idle_start > 10:
                out.append(
                    Verdict(
                        "warn",
                        f"Idle gap from {idle_start:.0f}s to {bucket_s}s "
                        f"({bucket_s - idle_start:.0f}s) — goroutines ≤2. "
                        f"Check for blocked I/O or missing parallelism.",
                    )
                )
            idle_start = None

    # 5. Excessive os/exec calls on single goroutine.
    by_goroutine: Counter[int] = Counter()
    by_goroutine_time: dict[int, float] = {}
    for e in exec_stats.top:
        by_goroutine[e.goroutine_id] += 1
        by_goroutine_time[e.goroutine_id] = by_goroutine_time.get(e.goroutine_id, 0) + e.duration_ms
    for gid, count in by_goroutine.most_common(3):
        total_t = by_goroutine_time.get(gid, 0)
        if count > 20 or total_t > 100:
            out.append(
                Verdict(
                    "warn",
                    f"Goroutine G{gid} spent {total_t:.0f}ms in "
                    f"{count} exec calls. Check for sequential "
                    f"container operations that could be parallelized.",
                )
            )

    # 6. Overall GC overhead.
    gc_pct = (gc.total_mark_ms / (duration_s * 1000)) * 100 if duration_s > 0 else 0
    if gc_pct > 10:
        out.append(
            Verdict(
                "warn",
                f"GC mark overhead is {gc_pct:.1f}% of total trace time.",
            )
        )

    if not out:
        out.append(Verdict("ok", "No anomalies detected."))

    return out


# ---------------------------------------------------------------------------
# Report formatting
# ---------------------------------------------------------------------------


def _section(title: str) -> None:
    print(f"\n{'=' * 60}")
    print(f"  {title}")
    print(f"{'=' * 60}")


def report_full(trace_path: str, data: dict[str, object]) -> None:
    events = _raw_events(data)

    ts_min = min((float(e.get("ts", 0)) for e in events), default=0)
    ts_max = max((float(e.get("ts", 0)) for e in events), default=0) + max(
        (float(e.get("dur", 0)) for e in events), default=0
    )
    duration_s = round((ts_max - ts_min) / 1e6, 1)

    gc = analyze_gc(events)
    gr = analyze_goroutines(events)
    http = analyze_http(events)
    exec_stats = analyze_exec(events)
    user_events = analyze_user_events(events)
    findings = verdicts(duration_s, gc, gr, http, exec_stats, user_events)

    # --- Header ---
    print(f"Trace: {trace_path}")
    print(f"Duration: {duration_s}s  |  Events: {len(events):,}")

    # --- Verdicts (most important, so first) ---
    _section("Verdicts")
    for v in findings:
        icon = {"ok": "✓", "warn": "⚠", "bad": "✗"}.get(v.level, "?")
        print(f"  {icon} [{v.level}] {v.message}")

    # --- GC ---
    _section("GC")
    print(f"  Marks: {gc.num_marks}  |  Total mark: {gc.total_mark_ms}ms")
    print(f"  Max single mark: {gc.max_mark_ms}ms")
    print(f"  STW: {gc.num_stw} pauses, {gc.total_stw_ms}ms total")
    if gc.marks:
        top5 = sorted(gc.marks, key=lambda m: -m.duration_ms)[:5]
        items = ", ".join(f"{m.duration_ms:.0f}ms @ {m.time_s:.1f}s" for m in top5)
        print(f"  Top 5: {items}")

    # --- Goroutines ---
    _section("Goroutines")
    print(f"  Peak concurrent: {gr.max_concurrent}")
    print("  Timeline (5s buckets):")
    prev = -1
    for bucket_s, count in gr.timeline_5s:
        if count != prev:
            bar = "█" * min(count, 50)
            print(f"    {bucket_s:5.0f}s  {bar} {count}")
            prev = count
    if gr.top_slices:
        print("\n  Top goroutine slices (>1ms):")
        for s in gr.top_slices[:10]:
            print(f"    {s.duration_ms:8.1f}ms  @ {s.time_s:8.3f}s  {s.name}")

    # --- HTTP ---
    _section("HTTP")
    if http.first_request_s is not None:
        print(f"  First request:  {http.first_request_s}s")
        print(f"  Total: {http.total_requests}  |  Longest: {http.longest_ms}ms")
    else:
        print("  No HTTP requests in trace")

    # --- os/exec ---
    if exec_stats.count > 0:
        _section("os/exec")
        print(f"  Calls: {exec_stats.count}  |  Total: {exec_stats.total_ms}ms")
        print(f"  Max single: {exec_stats.max_ms}ms")
        for e in exec_stats.top[:5]:
            print(f"    {e.duration_ms:8.1f}ms  @ {e.time_s:8.3f}s  G{e.goroutine_id}")

    # --- User events ---
    _section(f"User Log Events ({len(user_events)})")
    for ev in user_events:
        print(f"  {ev.time_s:8.3f}s  {ev.name}")

    print()


def report_gc(trace_path: str, data: dict[str, object]) -> None:
    events = _raw_events(data)
    gc = analyze_gc(events)
    print(f"Trace: {trace_path}")
    print(f"Marks: {gc.num_marks}  |  Total mark: {gc.total_mark_ms}ms  |  Max: {gc.max_mark_ms}ms")
    print(f"STW:   {gc.num_stw} pauses, {gc.total_stw_ms}ms total")
    if gc.marks:
        print("Mark timeline:")
        for m in gc.marks:
            bar = "█" * max(1, int(m.duration_ms / gc.max_mark_ms * 40))
            print(f"  {m.time_s:8.3f}s  {m.duration_ms:6.1f}ms  {bar}")


def report_goroutines(trace_path: str, data: dict[str, object]) -> None:
    events = _raw_events(data)
    gr = analyze_goroutines(events)
    print(f"Trace: {trace_path}")
    for bucket_s, count in gr.timeline_5s:
        bar = "█" * max(1, count)
        print(f"  {bucket_s:5.0f}s  {bar} {count}")
    print(f"\nPeak: {gr.max_concurrent}")
    if gr.top_slices:
        print("\nTop slices:")
        for s in gr.top_slices[:15]:
            print(f"  {s.duration_ms:8.1f}ms  @ {s.time_s:8.3f}s  {s.name}")


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main() -> int:
    parser = argparse.ArgumentParser(
        description=__doc__,
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    parser.add_argument("trace", help="Path to trace.out file")
    parser.add_argument("--gc", action="store_true", help="GC timeline report")
    parser.add_argument("--goroutines", action="store_true", help="Goroutine timeline report")
    parser.add_argument("--port", type=int, default=0, help="Port for trace server (default: auto)")
    args = parser.parse_args()

    if not os.path.exists(args.trace):
        print(f"Error: {args.trace} not found", file=sys.stderr)
        return 1

    print(f"Starting trace server for {args.trace}...", file=sys.stderr)
    proc, port = _start_trace_server(args.trace, args.port)
    try:
        data = _fetch_trace_json(port)
    finally:
        proc.send_signal(signal.SIGTERM)
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()

    if args.gc:
        report_gc(args.trace, data)
    elif args.goroutines:
        report_goroutines(args.trace, data)
    else:
        report_full(args.trace, data)

    return 0


if __name__ == "__main__":
    sys.exit(main())
