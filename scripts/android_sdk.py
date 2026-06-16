#!/usr/bin/env python3
"""Install Android SDK packages and set up the caic emulator."""

import argparse
import os
import platform
import re
import shutil
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
import zipfile

REPOSITORY_XML_URL = "https://dl.google.com/android/repository/repository2-1.xml"
DEFAULT_SDK_ROOT = os.path.expanduser("~/.local/share/android-sdk")
AVD_NAME = "caic_test"
DEVICE_PROFILE = "pixel_6"
YES_INPUT = b"y\ny\ny\ny\ny\ny\n"


def _common_sdk_roots() -> list[str]:
    """Return SDK locations used locally and in GitHub Actions."""
    roots: list[str] = []
    for var in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        value = os.environ.get(var)
        if value and value not in roots:
            roots.append(value)
    for root in (
        DEFAULT_SDK_ROOT,
        os.path.expanduser("~/Android/Sdk"),
        os.path.expanduser("~/Library/Android/sdk"),
        "/usr/local/lib/android/sdk",
    ):
        if root not in roots:
            roots.append(root)
    return roots


def _find_sdkmanager() -> tuple[str, str] | None:
    """Find sdkmanager and return it with its SDK root."""
    path = shutil.which("sdkmanager")
    if path:
        sdk_root = os.path.abspath(os.path.join(os.path.dirname(path), "..", "..", ".."))
        return path, sdk_root
    for root in _common_sdk_roots():
        sdk_root = os.path.abspath(os.path.expanduser(root))
        candidate = os.path.join(sdk_root, "cmdline-tools", "latest", "bin", "sdkmanager")
        if os.path.isfile(candidate):
            return candidate, sdk_root
    return None


def _platform_tag() -> str:
    """Return the Android command-line tools download tag."""
    system = platform.system()
    if system == "Linux":
        return "linux"
    if system == "Darwin":
        return "mac"
    raise RuntimeError(f"Unsupported platform: {system}")


def _system_image() -> str:
    """Return the emulator system image for the current host architecture."""
    machine = platform.machine()
    if machine in ("aarch64", "arm64"):
        return "system-images;android-35;google_apis;arm64-v8a"
    return "system-images;android-35;google_apis;x86_64"


def _fetch_latest_cmdline_tools_version() -> str:
    """Fetch the latest command-line tools version from Google's repository XML."""
    tag = _platform_tag()
    try:
        with urllib.request.urlopen(REPOSITORY_XML_URL, timeout=30) as response:
            xml = response.read().decode("utf-8", errors="replace")
    except OSError as exc:
        raise RuntimeError(f"Failed to fetch repository XML: {exc}") from exc
    block_pattern = re.compile(
        r'<remotePackage[^>]*path="cmdline-tools;latest"[^>]*>'
        r"(.*?)"
        r"</remotePackage>",
        re.DOTALL,
    )
    match = block_pattern.search(xml)
    if not match:
        raise RuntimeError("Could not find 'cmdline-tools;latest' in repository XML.")
    url_match = re.search(rf"commandlinetools-{tag}-(\d+)_latest\.zip", match.group(1))
    if not url_match:
        raise RuntimeError(f"Could not find {tag} cmdline-tools URL in repository XML.")
    return url_match.group(1)


def _make_executable_recursive(bin_dir: str) -> None:
    """Ensure all non-.bat files under bin_dir have the executable bit set."""
    for entry in os.scandir(bin_dir):
        if entry.is_file(follow_symlinks=False) and not entry.name.endswith(".bat"):
            os.chmod(entry.path, 0o755)


def _download_cmdline_tools(sdk_root: str) -> str:
    """Download command-line tools into sdk_root and return sdkmanager path."""
    tag = _platform_tag()
    version = _fetch_latest_cmdline_tools_version()
    print(f"Latest cmdline-tools version: {version}", file=sys.stderr)
    url = f"https://dl.google.com/android/repository/commandlinetools-{tag}-{version}_latest.zip"
    tools_dir = os.path.join(sdk_root, "cmdline-tools", "latest")
    os.makedirs(tools_dir, exist_ok=True)
    with tempfile.NamedTemporaryFile(suffix=".zip", delete=False) as temp_file:
        zip_path = temp_file.name
    try:
        print(f"Downloading {url} ...", file=sys.stderr)
        urllib.request.urlretrieve(url, zip_path)
        with tempfile.TemporaryDirectory(prefix="cmdline-tools-extract-") as extract_dir:
            with zipfile.ZipFile(zip_path, "r") as zip_file:
                zip_file.extractall(extract_dir)
            inner = os.path.join(extract_dir, "cmdline-tools")
            for name in os.listdir(inner):
                src = os.path.join(inner, name)
                dst = os.path.join(tools_dir, name)
                if os.path.isdir(dst):
                    shutil.rmtree(dst)
                elif os.path.exists(dst):
                    os.remove(dst)
                shutil.move(src, dst)
        print(f"Command-line tools installed to {tools_dir}", file=sys.stderr)
    except urllib.error.HTTPError as exc:
        raise RuntimeError(
            f"\nFailed to download cmdline-tools from {url} (HTTP {exc.code}).\n"
            "The repository XML may list a version not yet published.\n"
            "Report this if it persists: https://github.com/caic-xyz/caic/issues"
        ) from exc
    finally:
        if os.path.exists(zip_path):
            os.remove(zip_path)
    sdkmanager = os.path.join(tools_dir, "bin", "sdkmanager")
    _make_executable_recursive(os.path.join(tools_dir, "bin"))
    return sdkmanager


def _install_package(sdkmanager: str, sdk_root: str, package: str) -> int:
    """Install one SDK package, retrying on transient download failures."""
    env = {**os.environ, "ANDROID_HOME": sdk_root, "ANDROID_SDK_ROOT": sdk_root}
    max_retries = 3
    for attempt in range(max_retries):
        result = subprocess.run(
            [sdkmanager, f"--sdk_root={sdk_root}", "--install", package],
            env=env,
            input=YES_INPUT,
            capture_output=True,
            check=False,
        )
        if result.returncode == 0:
            return 0
        stderr = result.stderr.decode(errors="replace").strip()
        stdout = result.stdout.decode(errors="replace").strip()
        detail = stderr or stdout or "no sdkmanager output"
        if attempt < max_retries - 1:
            delay = 2**attempt
            print(
                f"sdkmanager failed to install {package} (attempt {attempt + 1}/{max_retries}):\n{detail}\n"
                f"Retrying in {delay}s...",
                file=sys.stderr,
            )
            # Clear the sdkmanager download cache to avoid reusing a corrupt zip.
            temp_dir = os.path.join(sdk_root, ".temp")
            if os.path.isdir(temp_dir):
                shutil.rmtree(temp_dir, ignore_errors=True)
            time.sleep(delay)
        else:
            print(
                f"sdkmanager failed to install {package} after {max_retries} attempts:\n{detail}",
                file=sys.stderr,
            )
            return 1
    return 1


def _install_missing_packages(sdkmanager: str, sdk_root: str, packages: dict[str, str]) -> int:
    """Install SDK packages whose package directories are missing."""
    for package, package_dir in packages.items():
        if not os.path.isdir(os.path.join(sdk_root, package_dir)):
            if _install_package(sdkmanager, sdk_root, package) != 0:
                return 1
    return 0


def _accept_licenses(sdkmanager: str, sdk_root: str) -> None:
    """Accept SDK licenses and sync repository metadata."""
    env = {**os.environ, "ANDROID_HOME": sdk_root, "ANDROID_SDK_ROOT": sdk_root}
    subprocess.run(
        [sdkmanager, f"--sdk_root={sdk_root}", "--licenses"],
        env=env,
        input=YES_INPUT,
        capture_output=True,
        check=False,
    )


def _check_host_supported() -> None:
    """Raise an error if this host cannot run the Android emulator."""
    system = platform.system()
    machine = platform.machine()
    if system == "Linux" and machine == "aarch64":
        raise RuntimeError(
            "The Android emulator is not available for ARM64 Linux.\n"
            "Options:\n"
            "  - Use an x86_64 Linux host\n"
            "  - Connect a physical ARM64 Android device via adb\n"
            "  - Run the emulator on macOS (Apple Silicon / Rosetta 2)\n"
            "  - Use GitHub Actions CI (ubuntu-latest runners are x86_64)"
        )


def _ensure_java() -> None:
    """Raise an error if no working Java runtime is available."""
    try:
        subprocess.run(["java", "-version"], capture_output=True, check=True)
    except (FileNotFoundError, subprocess.CalledProcessError, OSError) as exc:
        raise RuntimeError(
            "Java is required to run sdkmanager and avdmanager.\n"
            "Install a JDK (17+) then run 'java -version' to verify:\n"
            "  macOS:  brew install openjdk@21\n"
            "          sudo ln -sfn $(brew --prefix)/opt/openjdk@21/libexec/openjdk.jdk \\\n"
            "            /Library/Java/JavaVirtualMachines/openjdk-21.jdk\n"
            "  Linux:  sudo apt install openjdk-21-jdk"
        ) from exc


def _create_avd(sdkmanager: str, sdk_root: str) -> int:
    """Create the caic test AVD.  Writes the .ini ourselves if avdmanager doesn't."""
    avdmanager = os.path.join(os.path.dirname(sdkmanager), "avdmanager")
    if not os.path.isfile(avdmanager):
        print(f"avdmanager not found next to {sdkmanager}", file=sys.stderr)
        return 1

    avd_home = os.environ.get(
        "ANDROID_AVD_HOME",
        os.path.join(os.environ.get("ANDROID_SDK_HOME", os.path.expanduser("~/.android")), "avd"),
    )
    os.makedirs(avd_home, exist_ok=True)
    avd_path = os.path.join(avd_home, f"{AVD_NAME}.avd")

    env = {**os.environ, "ANDROID_HOME": sdk_root, "ANDROID_SDK_ROOT": sdk_root, "ANDROID_AVD_HOME": avd_home}
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
            "--path",
            avd_path,
            "--force",
        ],
        input=b"no\n",
        env=env,
        capture_output=True,
        check=False,
    )
    if result.returncode != 0:
        print(f"avdmanager failed (exit {result.returncode}):\n{result.stderr.decode()}", file=sys.stderr)
        return 1

    ini_path = os.path.join(avd_home, f"{AVD_NAME}.ini")
    if not os.path.isfile(ini_path):
        parts = _system_image().split(";")
        target = parts[1] if len(parts) > 1 else "android-35"
        with open(ini_path, "w") as f:
            f.write(f"path={avd_path}\n")
            f.write(f"target={target}\n")
        print(f"Wrote {ini_path}", file=sys.stderr)

    return 0


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
        lines += [f"fish_add_path {path}" for path in paths]
        return "\n".join(lines) + "\n"
    if "csh" in shell:
        path_str = " ".join(paths)
        return f"setenv ANDROID_HOME {sdk_root}\nset path=({path_str} $path)\n"
    lines = [f'export ANDROID_HOME="{sdk_root}"']
    for path in paths:
        lines.append(f'export PATH="{path}:$PATH"')
    return "\n".join(lines) + "\n"


def _check_packages() -> dict[str, str]:
    """Return SDK packages required by non-emulator Android checks."""
    return {
        "platforms;android-36": "platforms/android-36",
        "build-tools;36.0.0": "build-tools/36.0.0",
    }


def _emulator_packages() -> dict[str, str]:
    """Return SDK packages required by the local Android emulator."""
    system_image = _system_image()
    return {
        "platform-tools": "platform-tools",
        "emulator": "emulator",
        system_image: system_image.replace(";", os.sep),
    }


def _command_check() -> int:
    """Install non-emulator Android SDK packages."""
    sdk = _find_sdkmanager()
    if not sdk:
        searched = "\n  ".join(_common_sdk_roots())
        print(
            f"sdkmanager not found on PATH or under common SDK roots.\nCommon SDK roots:\n  {searched}",
            file=sys.stderr,
        )
        return 1
    sdkmanager, sdk_root = sdk
    return _install_missing_packages(sdkmanager, sdk_root, _check_packages())


def _command_setup_emulator() -> int:
    """Install emulator packages and create the caic test AVD."""
    _check_host_supported()
    _ensure_java()
    sdk = _find_sdkmanager()
    if sdk:
        sdkmanager, sdk_root = sdk
        print(f"Found sdkmanager at {sdkmanager}", file=sys.stderr)
    else:
        sdk_root = os.path.abspath(os.path.expanduser(_common_sdk_roots()[0]))
        print("sdkmanager not found; downloading command-line tools ...", file=sys.stderr)
        sdkmanager = _download_cmdline_tools(sdk_root)
    print("Accepting licenses and syncing repository ...", file=sys.stderr)
    _accept_licenses(sdkmanager, sdk_root)
    packages = _check_packages()
    packages.update(_emulator_packages())
    if _install_missing_packages(sdkmanager, sdk_root, packages) != 0:
        return 1
    print(f"Creating AVD '{AVD_NAME}' ...", file=sys.stderr)
    if _create_avd(sdkmanager, sdk_root) != 0:
        return 1
    print(f"\nEmulator '{AVD_NAME}' is ready.")
    if "ANDROID_HOME" not in os.environ and "ANDROID_SDK_ROOT" not in os.environ:
        print("\nAdd this to your shell profile to make the SDK persistent:")
        print(_build_export_block(sdk_root))
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    subcommands = parser.add_subparsers(dest="command", required=True)
    subcommands.add_parser("check", help="install SDK packages required by android-check")
    subcommands.add_parser("setup-emulator", help="install emulator packages and create the caic test AVD")
    args = parser.parse_args()
    try:
        if args.command == "check":
            return _command_check()
        if args.command == "setup-emulator":
            return _command_setup_emulator()
    except RuntimeError as exc:
        print(exc, file=sys.stderr)
        return 1
    parser.print_help(file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
