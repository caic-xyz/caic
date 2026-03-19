#!/bin/bash
# Generate Android screenshots for the documentation site.
#
# Usage: ./scripts/gen-android-screenshots.sh
#
# Prerequisites:
#   - Android emulator running: make android-start-emulator
#   - caic built with e2e tags: go build -tags e2e -o /tmp/caic-e2e ./backend/cmd/caic
#   - Android APK installed: make android-push
#
# This script:
#   1. Starts the fake backend
#   2. Configures the Android app via adb UI automation
#   3. Creates tasks via the API
#   4. Takes screenshots
#   5. Saves them to e2e/screenshots/ (same dir as Playwright screenshots)

set -eu

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
CAIC_DIR="$(dirname "$SCRIPT_DIR")"
SCREENSHOT_DIR="$CAIC_DIR/e2e/screenshots"
API="http://localhost:8090"

cleanup() {
	if [ -n "${BACKEND_PID:-}" ]; then
		kill "$BACKEND_PID" 2>/dev/null || true
	fi
}
trap cleanup EXIT

# Check prerequisites.
if ! adb devices | grep -q "device$"; then
	echo "No Android device/emulator connected. Run: make android-start-emulator" >&2
	exit 1
fi

if [ ! -f /tmp/caic-e2e ]; then
	echo "Building caic e2e binary..."
	cd "$CAIC_DIR" && go build -tags e2e,nolibopusfile -o /tmp/caic-e2e ./backend/cmd/caic
fi

# Start fake backend.
echo "Starting fake backend..."
/tmp/caic-e2e -http :8090 &>/tmp/caic-e2e.log &
BACKEND_PID=$!
sleep 2

if ! curl -sf "$API/api/v1/server/repos" >/dev/null; then
	echo "Backend failed to start. Check /tmp/caic-e2e.log" >&2
	exit 1
fi

# Set up port forwarding.
adb reverse tcp:8090 tcp:8090

# Create tasks before launching the app so they're ready when the list loads.
echo "Creating tasks..."
create_task() {
	curl -sf -X POST "$API/api/v1/tasks" \
		-H 'Content-Type: application/json' \
		-d "{\"initialPrompt\":{\"text\":\"$1\"},\"repos\":[{\"name\":\"$2\"}],\"harness\":\"fake\"}" >/dev/null
}

create_task "Fix token expiry bug in auth middleware" "clone"
create_task "Plan the rate limiting implementation for API endpoints" "clone"
create_task "Which storage backend should we use for session data?" "clone"
create_task "Update CI pipeline to run tests in parallel" "clone2"

echo "Waiting for tasks to settle..."
sleep 5

# Helper: parse [x1,y1][x2,y2] bounds and print center coordinates.
tap_node() {
	local COORDS
	COORDS=$(adb shell cat /sdcard/window_dump.xml | python3 -c "
import xml.etree.ElementTree as ET, sys, re
tree = ET.parse(sys.stdin)
for node in tree.iter('node'):
    if node.get('$1','') == '$2':
        m = re.findall(r'\d+', node.get('bounds',''))
        print(f'{(int(m[0])+int(m[2]))//2} {(int(m[1])+int(m[3]))//2}')
        break
")
	if [ -z "$COORDS" ]; then
		echo "ERROR: node $1=$2 not found" >&2
		exit 1
	fi
	adb shell input tap $COORDS
}

# Launch the app.
echo "Launching app..."
adb shell am force-stop com.fghbuild.caic
sleep 1
adb shell am start -n com.fghbuild.caic/.MainActivity
sleep 3

# Navigate to Settings.
echo "Configuring server URL..."
adb shell uiautomator dump /sdcard/window_dump.xml 2>/dev/null
tap_node content-desc Settings
sleep 2

# Add a server.
adb shell uiautomator dump /sdcard/window_dump.xml 2>/dev/null
tap_node text "Add Server"
sleep 2

# Find EditText fields (Name, URL).
adb shell uiautomator dump /sdcard/window_dump.xml 2>/dev/null
FIELDS=$(adb shell cat /sdcard/window_dump.xml | python3 -c "
import xml.etree.ElementTree as ET, sys, re
tree = ET.parse(sys.stdin)
for node in tree.iter('node'):
    if 'EditText' in node.get('class',''):
        m = re.findall(r'\d+', node.get('bounds',''))
        print(f'{(int(m[0])+int(m[2]))//2} {(int(m[1])+int(m[3]))//2}')
")
NAME_FIELD=$(echo "$FIELDS" | head -1)
URL_FIELD=$(echo "$FIELDS" | tail -1)

# Enter URL first, then name.
adb shell input tap $URL_FIELD
sleep 0.5
adb shell input text "http://localhost:8090"
sleep 0.5
adb shell input tap $NAME_FIELD
sleep 0.5
adb shell input text "Local"
sleep 2

# Go back to task list via the app's Back button.
adb shell uiautomator dump /sdcard/window_dump.xml 2>/dev/null
tap_node content-desc Back
sleep 4

# Screenshot 1: Task list.
echo "Taking screenshots..."
mkdir -p "$SCREENSHOT_DIR"
adb exec-out screencap -p > "$SCREENSHOT_DIR/android-task-list.png"
echo "  android-task-list.png"

# Convert to AVIF.
ffmpeg -y -i "$SCREENSHOT_DIR/android-task-list.png" -c:v libaom-av1 -still-picture 1 -crf 0 -b:v 0 \
	"$SCREENSHOT_DIR/android-task-list.avif"
rm "$SCREENSHOT_DIR/android-task-list.png"

# Tap the first task to open detail view.
adb shell uiautomator dump /sdcard/window_dump.xml 2>/dev/null
tap_node text "Fix token expiry bug in auth middleware"
sleep 2

# Screenshot 2: Task detail.
adb exec-out screencap -p > "$SCREENSHOT_DIR/android-task-detail.png"
echo "  android-task-detail.png"

# Convert to AVIF.
ffmpeg -y -i "$SCREENSHOT_DIR/android-task-detail.png" -c:v libaom-av1 -still-picture 1 -crf 0 -b:v 0 \
	"$SCREENSHOT_DIR/android-task-detail.avif"
rm "$SCREENSHOT_DIR/android-task-detail.png"

echo "Done."
