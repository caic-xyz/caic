#!/usr/bin/env python3
"""Run caic development server with a temporary config directory.

Uses 'go run' to build and run the server, with a temporary config.toml.
Supports --fake for testing without real containers.

If --http :0 is used, dynamically finds a free port and writes it to a file.
"""

import argparse
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile

ROOT_DIR = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def find_free_port():
    """Find a free port by binding to port 0."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("", 0))
        return s.getsockname()[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--http",
        default=":8080",
        help='HTTP listen address (default: ":8080", use ":0" for dynamic port)',
    )
    parser.add_argument(
        "--fake",
        action="store_true",
        help="Run with fake backend (no real containers)",
    )
    parser.add_argument(
        "--port-file",
        help="Write the actual port to this file (for dynamic ports)",
    )
    args = parser.parse_args()

    # Handle dynamic port allocation
    http_addr = args.http
    if http_addr == ":0":
        port = find_free_port()
        http_addr = f":{port}"
        if args.port_file:
            with open(args.port_file, "w") as f:
                f.write(str(port))

    tmp_dir = tempfile.mkdtemp(prefix="caic-dev-")
    try:
        config_path = os.path.join(tmp_dir, "config.toml")
        with open(config_path, "w") as f:
            f.write(f'[server]\nhttp = "{http_addr}"\n')

        cmd = ["go", "run"]
        if args.fake:
            cmd.extend(["-tags", "e2e"])
        cmd.extend(["./backend/cmd/caic", "-config-dir", tmp_dir])

        proc = subprocess.Popen(cmd, cwd=ROOT_DIR)

        def handle_signal(signum, frame):
            proc.terminate()

        signal.signal(signal.SIGINT, handle_signal)
        signal.signal(signal.SIGTERM, handle_signal)

        proc.wait()
        return proc.returncode
    finally:
        shutil.rmtree(tmp_dir, ignore_errors=True)


if __name__ == "__main__":
    sys.exit(main())
