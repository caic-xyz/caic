#!/usr/bin/env python3
# KittenTTS HTTP worker for the Go local-stack adapter.

import argparse
import json
import sys
import traceback
from dataclasses import dataclass
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

import numpy as np
from kittentts import KittenTTS


@dataclass(frozen=True)
class Request:
    text: str
    voice: str
    speed: float

    def __post_init__(self) -> None:
        if self.voice == "":
            raise ValueError("voice is required")

    @classmethod
    def from_json(cls, raw: dict[str, Any]) -> "Request":
        voice = raw.get("voice")
        if not isinstance(voice, str):
            raise ValueError("voice is required")
        speed = raw.get("speed")
        if speed is None:
            raise ValueError("speed is required")
        return cls(text=str(raw.get("text", "")), voice=voice, speed=float(speed))


class Handler(BaseHTTPRequestHandler):
    model: KittenTTS

    def do_POST(self) -> None:
        if self.path != "/synthesize":
            self.send_json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        try:
            request = self.read_request()
        except Exception as exc:  # noqa: BLE001
            self.send_json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": str(exc), "traceback": traceback.format_exc()})
            return

        stream = self.model.model.generate_stream(
            text=request.text,
            voice=request.voice,
            speed=request.speed,
            clean_text=True,
        )
        try:
            first_audio = next(stream, None)
        except Exception as exc:  # noqa: BLE001
            self.send_json(HTTPStatus.INTERNAL_SERVER_ERROR, {"error": str(exc), "traceback": traceback.format_exc()})
            return

        self.send_response(HTTPStatus.OK)
        self.send_header("Content-Type", "application/octet-stream")
        self.end_headers()
        try:
            if first_audio is not None:
                self.write_pcm(first_audio)
            for audio in stream:
                self.write_pcm(audio)
        except Exception as exc:  # noqa: BLE001
            print(f"synthesize stream failed: {exc}\n{traceback.format_exc()}", file=sys.stderr)

    def read_request(self) -> Request:
        raw_length = self.headers.get("Content-Length")
        if raw_length is None:
            raise ValueError("missing Content-Length")
        length = int(raw_length)
        if length < 0 or length > (1 << 20):
            raise ValueError(f"invalid Content-Length {length}")
        raw = json.loads(self.rfile.read(length))
        return Request.from_json(raw)

    def send_json(self, status: HTTPStatus, message: dict[str, Any]) -> None:
        payload = json.dumps(message, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def write_pcm(self, audio: np.ndarray) -> None:
        pcm = np.clip(audio, -1.0, 1.0)
        pcm = (pcm * 32767.0).astype("<i2", copy=False)
        self.wfile.write(pcm.tobytes())
        self.wfile.flush()

    def log_message(self, fmt: str, *args: Any) -> None:
        print(fmt % args, file=sys.stderr)


def write_control(message: dict[str, Any]) -> None:
    sys.stdout.write(json.dumps(message, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def main() -> int:
    parser = argparse.ArgumentParser(description="Run the caic KittenTTS HTTP worker.")
    parser.add_argument("--cache-dir", required=True, help="Directory for Hugging Face model cache.")
    args = parser.parse_args()

    Handler.model = KittenTTS("KittenML/kitten-tts-mini-0.8", cache_dir=args.cache_dir, backend="cpu")
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    host, port = server.server_address
    write_control({"kind": "ready", "url": f"http://{host}:{port}", "voices": Handler.model.available_voices})
    try:
        server.serve_forever()
    except Exception as exc:
        write_control({"kind": "error", "error": str(exc), "traceback": traceback.format_exc()})
    finally:
        write_control({"kind": "exit"})
    return 0


if __name__ == "__main__":
    sys.exit(main())
