#!/usr/bin/env python3
"""Set up the Android emulator for caic E2E tests.

Downloads the Android command-line tools if sdkmanager is not already
available on PATH or in common SDK locations. Then installs the
system image and creates an AVD named caic_test.

Works on Linux and macOS. Requires Java (for sdkmanager/avdmanager).
"""

import argparse
import os
import platform
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.request
import zipfile

# URL of Google's SDK repository XML that lists all available packages
# including cmdline-tools.
REPOSITORY_XML_URL = "https://dl.google.com/android/repository/repository2-1.xml"

# Google-suggested default SDK directory (used when none is configured).
DEFAULT_SDK_ROOT = os.path.expanduser("~/Android/Sdk")


def _system_image() -> str:
    """Return the system image package for the current host architecture."""
    machine = platform.machine()
    if machine in ("aarch64", "arm64"):
        return "system-images;android-35;google_apis;arm64-v8a"
    return "system-images;android-35;google_apis;x86_64"


def _required_packages() -> list[str]:
    """Return the SDK packages required to run the emulator."""
    return sorted(
        [
            "platform-tools",
            "emulator",
            _system_image(),
        ]
    )


# AVD configuration.
AVD_NAME = "caic_test"
DEVICE_PROFILE = "pixel_6"


def _env_sdk_root() -> str | None:
    """Return the SDK root from environment variables, if set."""
    for var in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        val = os.environ.get(var)
        if val and os.path.isdir(val):
            return val
    return None


def _common_sdk_roots() -> list[str]:
    """Return a list of directories where the Android SDK is commonly located."""
    roots: list[str] = []
    env = _env_sdk_root()
    if env:
        roots.append(env)

    for candidate in (
        DEFAULT_SDK_ROOT,
        os.path.expanduser("~/Library/Android/sdk"),  # macOS
        "/usr/local/lib/android/sdk",  # GitHub Actions
    ):
        if candidate not in roots:
            roots.append(candidate)
    return roots


def find_sdkmanager() -> str | None:
    """Return the path to sdkmanager if found, or None."""
    # 1. Check PATH.
    path = shutil.which("sdkmanager")
    if path:
        return path

    # 2. Check common SDK locations (cmdline-tools/latest/bin/).
    for root in _common_sdk_roots():
        candidate = os.path.join(root, "cmdline-tools", "latest", "bin", "sdkmanager")
        if os.path.isfile(candidate):
            return candidate

    return None


def _check_host_supported() -> None:
    """Exit if the host can't run the Android emulator.

    The emulator ships x86_64 binaries for Linux and x86_64/arm64 binaries
    for macOS.  arm64 Linux has no emulator build and must use a physical
    device or a remote emulator instead.
    """
    system = platform.system()
    machine = platform.machine()

    if system == "Linux" and machine == "aarch64":
        sys.exit(
            "The Android emulator is not available for ARM64 Linux.\n"
            "Options:\n"
            "  - Use an x86_64 Linux host\n"
            "  - Connect a physical ARM64 Android device via adb\n"
            "  - Run the emulator on macOS (Apple Silicon / Rosetta 2)\n"
            "  - Use GitHub Actions CI (ubuntu-latest runners are x86_64)\n"
        )


def _platform_tag() -> str:
    """Return the Android download tag for the current OS."""
    system = platform.system()
    if system == "Linux":
        return "linux"
    if system == "Darwin":
        return "mac"
    sys.exit(f"Unsupported platform: {system}")


def _fetch_latest_cmdline_tools_version() -> str:
    """Fetch the latest cmdline-tools version from Google's repository XML.

    The XML contains a <remotePackage path="cmdline-tools;latest"> entry
    with platform-specific URLs like:
        commandlinetools-linux-14742923_latest.zip
    We extract the version number from the URL matching the current OS.
    """
    tag = _platform_tag()
    try:
        with urllib.request.urlopen(REPOSITORY_XML_URL, timeout=30) as resp:
            xml_bytes = resp.read()
    except OSError as exc:
        sys.exit(f"Failed to fetch repository XML: {exc}")

    # Find the cmdline-tools;latest block and extract the version for this OS.
    # The XML is big (~80KB), so use regex instead of an XML parser for speed.
    # Pattern: <remotePackage path="cmdline-tools;latest"> ... <url>...-{tag}-(\d+)_latest.zip</url>
    block_pattern = re.compile(
        r'<remotePackage[^>]*path="cmdline-tools;latest"[^>]*>'
        r"(.*?)"
        r"</remotePackage>",
        re.DOTALL,
    )
    match = block_pattern.search(xml_bytes.decode("utf-8", errors="replace"))
    if not match:
        sys.exit("Could not find 'cmdline-tools;latest' in repository XML.")

    url_pattern = re.compile(rf"commandlinetools-{tag}-(\d+)_latest\.zip")
    url_match = url_pattern.search(match.group(1))
    if not url_match:
        sys.exit(f"Could not find {tag} cmdline-tools URL in repository XML.")

    return url_match.group(1)


def download_cmdline_tools(sdk_root: str) -> str:
    """Download and extract Android command-line tools into sdk_root.

    Returns the path to sdkmanager.
    """
    tag = _platform_tag()
    version = _fetch_latest_cmdline_tools_version()
    print(f"Latest cmdline-tools version: {version}", file=sys.stderr)
    url = f"https://dl.google.com/android/repository/commandlinetools-{tag}-{version}_latest.zip"
    tools_dir = os.path.join(sdk_root, "cmdline-tools", "latest")
    os.makedirs(tools_dir, exist_ok=True)

    with tempfile.NamedTemporaryFile(suffix=".zip", delete=False) as tf:
        zip_path = tf.name

    try:
        print(f"Downloading {url} ...", file=sys.stderr)
        urllib.request.urlretrieve(url, zip_path)

        with zipfile.ZipFile(zip_path, "r") as zf:
            # The zip contains a top-level cmdline-tools/ directory.
            # Extract to a temp location, then move contents into tools_dir.
            extract_dir = tempfile.mkdtemp(prefix="cmdline-tools-extract-")
            zf.extractall(extract_dir)

        inner = os.path.join(extract_dir, "cmdline-tools")
        for name in os.listdir(inner):
            src = os.path.join(inner, name)
            dst = os.path.join(tools_dir, name)
            if os.path.exists(dst):
                if os.path.isdir(dst):
                    shutil.rmtree(dst)
                else:
                    os.remove(dst)
            shutil.move(src, dst)

        print(f"Command-line tools installed to {tools_dir}", file=sys.stderr)
    except urllib.error.HTTPError as exc:
        sys.exit(
            f"\nFailed to download cmdline-tools from {url} (HTTP {exc.code}).\n"
            "The repository XML may list a version not yet published.\n"
            "Report this if it persists: https://github.com/caic-xyz/caic/issues\n"
        )
    finally:
        if os.path.exists(zip_path):
            os.remove(zip_path)

    sdkmanager = os.path.join(tools_dir, "bin", "sdkmanager")
    # Make the tools executable (zipfile does not preserve permissions).
    _make_executable_recursive(os.path.join(tools_dir, "bin"))
    return sdkmanager


def _make_executable_recursive(bin_dir: str) -> None:
    """Ensure all non-.bat files under bin_dir have the executable bit set."""
    for entry in os.scandir(bin_dir):
        if entry.is_file(follow_symlinks=False) and not entry.name.endswith(".bat"):
            os.chmod(entry.path, 0o755)


_JAVA_INSTALL_MSG = (
    "Java is required to run sdkmanager and avdmanager.\n"
    "Install a JDK (17+) then run 'java -version' to verify:\n"
    "  macOS:  brew install openjdk@21\n"
    "          sudo ln -sfn $(brew --prefix)/opt/openjdk@21/libexec/openjdk.jdk \\\n"
    "            /Library/Java/JavaVirtualMachines/openjdk-21.jdk\n"
    "  Linux:  sudo apt install openjdk-21-jdk"
)


def _ensure_java() -> None:
    """Exit with an error if no working Java runtime is available."""
    try:
        subprocess.run(["java", "-version"], capture_output=True, check=True)
    except (FileNotFoundError, subprocess.CalledProcessError, OSError):
        sys.exit(_JAVA_INSTALL_MSG)


def _run_sdkmanager(sdkmanager: str, sdk_root: str, *args: str, exit_on_error: bool = True) -> None:
    """Run sdkmanager with the given arguments, passing yes to accept licenses."""
    env = os.environ.copy()
    env["ANDROID_HOME"] = sdk_root
    env["ANDROID_SDK_ROOT"] = sdk_root
    result = subprocess.run(
        [sdkmanager, f"--sdk_root={sdk_root}", *args],
        env=env,
        input=b"y\ny\ny\ny\ny\ny\n",  # accept all license prompts
        capture_output=True,
    )
    if exit_on_error and result.returncode != 0:
        sys.exit(f"sdkmanager failed (exit {result.returncode}):\n{result.stderr.decode()}")


def setup_emulator(sdkmanager: str, sdk_root: str) -> None:
    """Install the system image and create the AVD."""
    avdmanager = sdkmanager.replace("sdkmanager", "avdmanager")

    # On a fresh SDK root, sdkmanager needs to download the repository
    # manifest before it can resolve dependencies.  Accepting licenses
    # triggers this (it fetches the full package list first).
    print("Accepting licenses and syncing repository ...", file=sys.stderr)
    _run_sdkmanager(sdkmanager, sdk_root, "--licenses", exit_on_error=False)

    for pkg in _required_packages():
        print(f"Installing: {pkg}", file=sys.stderr)
        _run_sdkmanager(sdkmanager, sdk_root, "--install", pkg)

    print(f"Creating AVD '{AVD_NAME}' ...", file=sys.stderr)
    result = subprocess.run(
        [
            avdmanager,
            "create",
            "avd",
            "-n",
            AVD_NAME,
            "-k",
            _system_image(),
            "-d",
            DEVICE_PROFILE,
            "--force",
        ],
        input=b"no\n",
        env={**os.environ, "ANDROID_HOME": sdk_root, "ANDROID_SDK_ROOT": sdk_root},
        capture_output=True,
    )
    if result.returncode != 0:
        sys.exit(f"avdmanager failed (exit {result.returncode}):\n{result.stderr.decode()}")


def _build_export_block(sdk_root: str) -> str:
    """Return shell commands to set ANDROID_HOME and extend PATH."""
    shell = os.environ.get("SHELL", "/bin/bash")
    paths = [
        os.path.join(sdk_root, "cmdline-tools", "latest", "bin"),
        os.path.join(sdk_root, "platform-tools"),
        os.path.join(sdk_root, "emulator"),
    ]
    if "fish" in shell:
        lines = [f"set -x ANDROID_HOME {sdk_root}"]
        lines += [f"fish_add_path {p}" for p in paths]
        return "\n".join(lines) + "\n"
    if "csh" in shell:
        path_str = " ".join(paths)
        return f"setenv ANDROID_HOME {sdk_root}\nset path=({path_str} $path)\n"
    lines = [f'export ANDROID_HOME="{sdk_root}"']
    for p in paths:
        lines.append(f'export PATH="{p}:$PATH"')
    return "\n".join(lines) + "\n"


def main() -> int:
    argparse.ArgumentParser(description=__doc__).parse_args()

    _check_host_supported()
    _ensure_java()

    sdkmanager_path = find_sdkmanager()
    if sdkmanager_path:
        # Determine SDK root from the discovered path.
        # Path looks like: .../cmdline-tools/latest/bin/sdkmanager
        sdk_root = os.path.normpath(os.path.join(os.path.dirname(sdkmanager_path), "..", "..", ".."))
        print(f"Found sdkmanager at {sdkmanager_path}", file=sys.stderr)
    else:
        print("sdkmanager not found; downloading command-line tools ...", file=sys.stderr)
        sdk_root = _env_sdk_root() or DEFAULT_SDK_ROOT
        sdkmanager_path = download_cmdline_tools(sdk_root)

    setup_emulator(sdkmanager_path, sdk_root)

    print(f"\nEmulator '{AVD_NAME}' is ready.")
    if "ANDROID_HOME" not in os.environ and "ANDROID_SDK_ROOT" not in os.environ:
        print("\nAdd this to your shell profile to make the SDK persistent:")
        print(_build_export_block(sdk_root))

    return 0


if __name__ == "__main__":
    sys.exit(main())
