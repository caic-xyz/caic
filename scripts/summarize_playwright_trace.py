#!/usr/bin/env python3
"""Summarize Playwright trace archives into a short, ordered failure report."""

import argparse
import json
import re
import sys
import zipfile
from collections.abc import Iterator
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

ANSI = re.compile(r"\x1b\[[0-9;]*m")
ERROR_LINES = 6
TRACE_SUFFIXES = (".trace", ".network")


@dataclass
class Request:
    time: float
    method: str
    url: str
    status: int
    duration_ms: float
    resource_type: str
    failure: str

    def noteworthy(self) -> bool:
        """Report documents, failures, and error statuses only; assets are noise."""
        return self.resource_type == "document" or self.status <= 0 or self.status >= 400 or bool(self.failure)


@dataclass
class Console:
    time: float
    level: str
    text: str
    location: str


@dataclass
class Log:
    time: float
    message: str


@dataclass
class Page:
    page_id: str
    browser: str
    url: str = ""
    start: float = 0.0
    requests: list[Request] = field(default_factory=list)
    console: list[Console] = field(default_factory=list)
    logs: list[Log] = field(default_factory=list)


def trace_files(path: Path) -> Iterator[tuple[str, bytes]]:
    """Yield each trace member of an archive or an unzipped trace directory."""
    if path.is_dir():
        for member in sorted(path.iterdir()):
            if member.name.endswith(TRACE_SUFFIXES):
                yield member.name, member.read_bytes()
        return
    with zipfile.ZipFile(path) as archive:
        for name in sorted(archive.namelist()):
            if name.endswith(TRACE_SUFFIXES):
                yield name, archive.read(name)


def events(source: bytes) -> Iterator[dict[str, Any]]:
    """Decode newline-delimited trace events, keeping only object records."""
    for line in source.splitlines():
        if not line.strip():
            continue
        event = json.loads(line)
        if isinstance(event, dict):
            yield event


def group_traces(files: Iterator[tuple[str, bytes]]) -> tuple[str, dict[str, Page]]:
    """Return the test error and the pages of one archive, keyed by page id."""
    error = ""
    pages: dict[str, Page] = {}
    for name, source in files:
        if Path(name).name == "test.trace":
            error = test_error(source)
            continue
        browser = ""
        page_id = ""
        logs: list[Log] = []
        console: list[Console] = []
        for event in events(source):
            match event.get("type"):
                case "context-options":
                    browser = event.get("browserName") or browser
                case "event":
                    if event.get("method") == "page":
                        page_id = event.get("params", {}).get("pageId", page_id)
                case "log":
                    logs.append(Log(time=float(event["time"]), message=event.get("message", "")))
                case "console":
                    location = event.get("location") or {}
                    console.append(
                        Console(
                            time=float(event["time"]),
                            level=event.get("messageType", ""),
                            text=event.get("text", ""),
                            location=location.get("url", ""),
                        )
                    )
        requests, page_id = network_requests(source, page_id) if name.endswith(".network") else ([], page_id)
        if not page_id:
            # Contexts without a page hold API requests; the owning page logs them.
            continue
        page = pages.setdefault(page_id, Page(page_id=page_id, browser=browser))
        page.browser = browser or page.browser
        page.requests.extend(requests)
        page.console.extend(console)
        page.logs.extend(logs)
    for page in pages.values():
        times = [
            t
            for t in (*(r.time for r in page.requests), *(c.time for c in page.console), *(lv.time for lv in page.logs))
            if t
        ]
        page.start = min(times) if times else 0.0
        documents = [r for r in page.requests if r.resource_type == "document"]
        if documents:
            page.url = documents[0].url
    return error, pages


def test_error(source: bytes) -> str:
    """Return the first test error message, without ANSI escapes."""
    for event in events(source):
        if event.get("type") == "error":
            return ANSI.sub("", str(event.get("message", "")))
    return ""


def network_requests(source: bytes, page_id: str) -> tuple[list[Request], str]:
    """Decode the HAR-style network log, returning its requests and page id."""
    requests: list[Request] = []
    for event in events(source):
        if event.get("type") != "resource-snapshot":
            continue
        snapshot = event["snapshot"]
        request = snapshot.get("request") or {}
        response = snapshot.get("response") or {}
        time = float(snapshot.get("_monotonicTime") or 0.0)
        page_id = snapshot.get("pageref") or page_id
        duration = snapshot.get("time")
        requests.append(
            Request(
                time=time,
                method=request.get("method", ""),
                url=request.get("url", ""),
                status=int(response.get("status") or 0),
                duration_ms=float(duration) if isinstance(duration, (int, float)) and duration >= 0 else -1.0,
                resource_type=snapshot.get("_resourceType", ""),
                failure=str(snapshot.get("_failureText") or ""),
            )
        )
    return requests, page_id


def _offset(time: float, start: float) -> str:
    """Render a millisecond trace timestamp as seconds since the page started."""
    return f"{(time - start) / 1000:+8.3f}s"


def describe_request(request: Request) -> str:
    """Render one request as a status, duration, and failure detail."""
    if request.status > 0:
        status = f"{request.status}"
        detail = f" {request.duration_ms:.0f}ms" if request.duration_ms >= 0 else ""
    else:
        status = "no response"
        detail = ""
    if request.failure:
        detail += f" {request.failure}"
    return f"{request.method} {request.url} -> {status}{detail}"


def report(path: Path, log_limit: int) -> None:
    """Print the summary of one trace archive."""
    error, pages = group_traces(trace_files(path))
    if not pages and not error:
        print(f"{path}: no trace members", file=sys.stderr)
        return
    print(f"=== {path}")
    if error:
        if "browserType.launch" in error and re.search(
            r"FATAL:[^\n]*\bextensions/|(?:failed to|cannot|unable to) initialize extensions"
            r"|extensions? initialization failed",
            error,
            re.IGNORECASE,
        ):
            print(
                "environment: Chromium failed during extension initialization, before product assertions. "
                "The sandbox must permit Playwright Chromium to start with its extension support. "
                "Verify the pinned browser and OS dependencies with "
                "`pnpm exec playwright install --with-deps chromium`; see DEV.md."
            )
        for line in [line for line in error.splitlines() if line.strip()][:ERROR_LINES]:
            print(f"error: {line}")
    for page in sorted(pages.values(), key=lambda p: p.start):
        header = f"page {page.page_id} ({page.browser or 'unknown'})"
        print(f"{header} {page.url}".rstrip())
        entries = [(r.time, "request", describe_request(r)) for r in page.requests if r.noteworthy()]
        entries.extend(
            (c.time, c.level or "console", f"{c.text} ({c.location})" if c.location else c.text) for c in page.console
        )
        for time, kind, text in sorted(entries):
            print(f"  {_offset(time, page.start)}  {kind:7s}  {text}")
        shown = page.logs[-log_limit:] if log_limit else []
        if shown:
            print("  logs:")
            if len(page.logs) > len(shown):
                print(f"  {_offset(shown[0].time, page.start)}  {len(page.logs) - len(shown)} earlier lines omitted")
            for log in shown:
                print(f"  {_offset(log.time, page.start)}           {log.message}")


def collect_archives(paths: list[str]) -> list[Path]:
    """Resolve files, trace directories, and directories holding trace archives."""
    archives: list[Path] = []
    for raw in paths:
        path = Path(raw)
        if path.is_dir() and (path / "test.trace").is_file():
            archives.append(path)
        elif path.is_dir():
            archives.extend(sorted(path.glob("**/trace.zip")))
        else:
            archives.append(path)
    return archives


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "paths",
        nargs="+",
        metavar="PATH",
        help="trace.zip, an unzipped trace directory, or a directory holding trace.zip archives",
    )
    parser.add_argument(
        "--logs", type=int, default=200, metavar="N", help="maximum log lines per page, 0 to disable (default: 200)"
    )
    args = parser.parse_args()
    if args.logs < 0:
        parser.error("--logs must not be negative")
    archives = collect_archives(args.paths)
    if not archives:
        print("no Playwright trace found", file=sys.stderr)
        return 1
    for archive in archives:
        report(archive, args.logs)
    return 0


if __name__ == "__main__":
    sys.exit(main())
