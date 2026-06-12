#!/usr/bin/env -S uv run --script
# Run the Brilliant Labs Halo Lua emulator with uv-managed dependencies.
# /// script
# requires-python = ">=3.10"
# dependencies = [
#   "brilliant-msg>=7.0.0",
#   "halo-emulator",
#   "websockets>=15.0.0",
# ]
#
# [tool.uv.sources]
# halo-emulator = { git = "https://github.com/brilliantlabsAR/brilliant_sdk.git", subdirectory = "python/packages/halo_emulator", rev = "b644147eb14021c297f6533d1b38b7e063b399f0" }  # noqa: E501
# ///

from __future__ import annotations

import argparse
import asyncio
import base64
import io
import json
import sys
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import websockets
from halo_emulator import HaloEmulator
from halo_emulator.cli import main as halo_cli_main
from websockets.asyncio.server import ServerConnection

JsonObject = dict[str, Any]


@dataclass(frozen=True)
class BridgeArgs:
    bind_host: str
    bind_port: int
    app_dir: Path | None
    script: str
    sandbox: Path | None


class HaloBridge:
    def __init__(self, emulator: HaloEmulator) -> None:
        self._emulator = emulator
        self._clients: set[ServerConnection] = set()
        self._lock = asyncio.Lock()
        self._loop: asyncio.AbstractEventLoop | None = None
        self._emulator._bluetooth.add_send_listener(self._on_bluetooth_send)

    def attach_loop(self, loop: asyncio.AbstractEventLoop) -> None:
        self._loop = loop

    def print_handler(self, text: str) -> None:
        self._schedule_event({"event": "print", "text": text})

    async def handle_client(self, websocket: ServerConnection) -> None:
        self._clients.add(websocket)
        try:
            async for message in websocket:
                await websocket.send(json.dumps(await self._handle_message(message)))
        finally:
            self._clients.remove(websocket)

    async def _handle_message(self, message: str | bytes) -> JsonObject:
        try:
            request = self._decode_request(message)
            request_id = request.get("id")
            op = request.get("op")
            if not isinstance(op, str):
                raise ValueError("request must include string field 'op'")
            result = await self._dispatch(op, request)
            return {"id": request_id, "ok": True, **result}
        except Exception as exc:  # noqa: BLE001 - Error boundary for remote requests.
            request_id = request.get("id") if "request" in locals() else None
            return {"id": request_id, "ok": False, "error": str(exc)}

    async def _dispatch(self, op: str, request: JsonObject) -> JsonObject:
        operations: dict[str, Callable[[JsonObject], Awaitable[JsonObject]]] = {
            "break": self._break,
            "button_double": self._button_double,
            "button_long": self._button_long,
            "button_single": self._button_single,
            "clear_display": self._clear_display,
            "connect_repl": self._connect_repl,
            "execute_lua": self._execute_lua,
            "get_framebuffer": self._get_framebuffer,
            "imu_tap": self._imu_tap,
            "ping": self._ping,
            "remove_all_files": self._remove_all_files,
            "reset": self._reset,
            "send_message": self._send_message,
            "start": self._start,
            "stop": self._stop,
            "upload_file": self._upload_file,
        }
        handler = operations.get(op)
        if handler is None:
            raise ValueError(f"unsupported op: {op}")
        async with self._lock:
            return await handler(request)

    def _decode_request(self, message: str | bytes) -> JsonObject:
        if isinstance(message, bytes):
            message = message.decode("utf-8")
        value = json.loads(message)
        if not isinstance(value, dict):
            raise ValueError("request must be a JSON object")
        return value

    async def _ping(self, request: JsonObject) -> JsonObject:
        return {"running": self._emulator.is_running(), "error": self._error_text()}

    async def _connect_repl(self, request: JsonObject) -> JsonObject:
        if self._emulator.is_running():
            self._emulator.stop()
        self._emulator.connect()
        return {}

    async def _execute_lua(self, request: JsonObject) -> JsonObject:
        code = self._required_str(request, "code")
        result = self._emulator.execute_lua(code)
        return {"result": None if result is None else str(result)}

    async def _start(self, request: JsonObject) -> JsonObject:
        script = request.get("script", "main.lua")
        if not isinstance(script, str):
            raise ValueError("script must be a string")
        if self._emulator.is_running():
            self._emulator.stop()
        self._emulator.start(script_name=script)
        return {}

    async def _stop(self, request: JsonObject) -> JsonObject:
        self._emulator.stop()
        return {"error": self._error_text()}

    async def _break(self, request: JsonObject) -> JsonObject:
        self._emulator.stop()
        return {}

    async def _reset(self, request: JsonObject) -> JsonObject:
        self._emulator.stop()
        main_lua = self._emulator._sandbox_dir / "main.lua"
        if main_lua.exists():
            self._emulator.start(script_name="main.lua")
        else:
            self._emulator.connect()
        return {"started": main_lua.exists()}

    async def _remove_all_files(self, request: JsonObject) -> JsonObject:
        self._emulator.stop()
        for path in self._emulator._sandbox_dir.iterdir():
            if path.is_file():
                path.unlink()
        self._emulator.connect()
        return {}

    async def _upload_file(self, request: JsonObject) -> JsonObject:
        path = self._required_str(request, "path")
        content = self._required_str(request, "content")
        destination = (self._emulator._sandbox_dir / path).resolve()
        sandbox = self._emulator._sandbox_dir.resolve()
        if sandbox not in destination.parents and destination != sandbox:
            raise ValueError(f"path escapes sandbox: {path}")
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_text(content, encoding="utf-8")
        return {}

    async def _clear_display(self, request: JsonObject) -> JsonObject:
        if self._emulator.is_running():
            raise ValueError("cannot clear display while a Lua script is running")
        if self._emulator._lua is None:
            self._emulator.connect()
        self._emulator.execute_lua("frame.display.clear(0)")
        return {}

    async def _send_message(self, request: JsonObject) -> JsonObject:
        msg_code = request.get("msgCode")
        if not isinstance(msg_code, int) or not 0 <= msg_code <= 255:
            raise ValueError("msgCode must be an integer in range 0..255")
        payload = self._decode_base64(self._required_str(request, "payload"))
        if len(payload) > 65535:
            raise ValueError("payload must fit in 65535 bytes")
        packet = bytes([msg_code, len(payload) >> 8, len(payload) & 0xFF]) + payload
        self._emulator.inject_bluetooth_data(packet)
        return {}

    async def _button_single(self, request: JsonObject) -> JsonObject:
        self._emulator.inject_button_single()
        return {}

    async def _button_double(self, request: JsonObject) -> JsonObject:
        self._emulator.inject_button_double()
        return {}

    async def _button_long(self, request: JsonObject) -> JsonObject:
        self._emulator.inject_button_long()
        return {}

    async def _imu_tap(self, request: JsonObject) -> JsonObject:
        self._emulator.inject_imu_tap()
        return {}

    async def _get_framebuffer(self, request: JsonObject) -> JsonObject:
        output = io.BytesIO()
        self._emulator.get_framebuffer().save(output, format="PNG")
        return {"imagePng": base64.b64encode(output.getvalue()).decode("ascii")}

    def _on_bluetooth_send(self, data: bytes) -> None:
        self._schedule_event({"event": "bluetooth_sent", "data": self._encode_base64(data)})

    def _schedule_event(self, event: JsonObject) -> None:
        loop = self._loop
        if loop is None:
            return
        asyncio.run_coroutine_threadsafe(self._broadcast(event), loop)

    async def _broadcast(self, event: JsonObject) -> None:
        if not self._clients:
            return
        encoded = json.dumps(event)
        await asyncio.gather(*(client.send(encoded) for client in self._clients), return_exceptions=True)

    def _error_text(self) -> str | None:
        error = self._emulator.get_error()
        return None if error is None else str(error)

    def _required_str(self, request: JsonObject, key: str) -> str:
        value = request.get(key)
        if not isinstance(value, str):
            raise ValueError(f"{key} must be a string")
        return value

    def _decode_base64(self, value: str) -> bytes:
        return base64.b64decode(value.encode("ascii"), validate=True)

    def _encode_base64(self, value: bytes) -> str:
        return base64.b64encode(value).decode("ascii")


async def run_bridge(args: BridgeArgs) -> None:
    emulator = HaloEmulator(sandbox_dir=args.sandbox or args.app_dir)
    bridge = HaloBridge(emulator)
    bridge.attach_loop(asyncio.get_running_loop())
    emulator._print_handler = bridge.print_handler

    if args.app_dir is not None:
        emulator.load_directory(args.app_dir)
        emulator.start(script_name=args.script)
    else:
        emulator.connect()

    async with websockets.serve(bridge.handle_client, args.bind_host, args.bind_port):
        print(f"[halo-emulator] Bridge listening on ws://{args.bind_host}:{args.bind_port}")
        await asyncio.Future()


def parse_bridge_args(argv: list[str]) -> BridgeArgs:
    parser = argparse.ArgumentParser(description="Halo smart glasses Lua emulator bridge")
    parser.add_argument("app_dir", nargs="?", default=None, help="Directory containing Lua app files")
    parser.add_argument("--script", default="main.lua", metavar="NAME", help="Entry-point Lua filename")
    parser.add_argument("--sandbox", default=None, metavar="DIR", help="Explicit sandbox directory")
    parser.add_argument("--bridge", required=True, metavar="HOST:PORT", help="WebSocket bridge bind address")
    args = parser.parse_args(argv)
    host, port = parse_bind(args.bridge)
    return BridgeArgs(
        bind_host=host,
        bind_port=port,
        app_dir=None if args.app_dir is None else Path(args.app_dir),
        script=args.script,
        sandbox=None if args.sandbox is None else Path(args.sandbox),
    )


def parse_bind(value: str) -> tuple[str, int]:
    if ":" not in value:
        raise ValueError("--bridge must be HOST:PORT")
    host, port_text = value.rsplit(":", 1)
    if not host:
        host = "127.0.0.1"
    return host, int(port_text)


def main(argv: list[str]) -> int:
    if "--bridge" not in argv:
        halo_cli_main()
        return 0
    asyncio.run(run_bridge(parse_bridge_args(argv)))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
