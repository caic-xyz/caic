# Build, benchmark, test, lint, and development workflow targets for the full stack (Go backend, TypeScript frontend, Android).

.PHONY: help benchmark build check check-agent-logs fake-dev test test-all smoke smoke-voice coverage lint lint-check lint-go lint-frontend lint-python lint-kotlin lint-binaries lint-docs format format-check format-kotlin verify refresh-generated generate-sdks git-hooks frontend-build frontend-dev upgrade frontend-e2e playwright-browser screenshots-check screenshots-check-frontend screenshots-check-android screenshots-generate-frontend screenshots-generate-android screenshots-update android-sdk android-check android-push-gomode android-e2e android-setup-emulator android-start-emulator android-stop-emulator tools

# Tool versions. The tools target installs a tool that is missing or at another version, so
# these are the only places the versions are written down.
GOLANGCI_LINT_VERSION=v2.13.2
SHFMT_VERSION=v3.14.1
RUFF_VERSION=0.16.8

# The tools target installs into the Go and uv tool directories. Prepend them so a recipe
# that just installed a tool can run it, whatever the caller's PATH holds.
GO_BIN := $(if $(shell command -v go 2>/dev/null),$(shell go env GOPATH 2>/dev/null)/bin)
UV_BIN := $(if $(shell command -v uv 2>/dev/null),$(shell uv tool dir --bin 2>/dev/null))
export PATH := $(if $(GO_BIN),$(GO_BIN):)$(if $(UV_BIN),$(UV_BIN):)$(PATH)

tools:
	@command -v golangci-lint > /dev/null 2>&1 && golangci-lint --version 2>/dev/null | grep -Fqw "$(GOLANGCI_LINT_VERSION:v%=%)" || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@command -v shfmt > /dev/null 2>&1 && shfmt --version 2>/dev/null | grep -Fqw "$(SHFMT_VERSION)" || go install mvdan.cc/sh/v3/cmd/shfmt@$(SHFMT_VERSION)
	@command -v uv > /dev/null 2>&1 || { echo 'uv is required to install the Python tools; see https://docs.astral.sh/uv/' >&2; exit 1; }
	@ruff --version 2>/dev/null | grep -Fqw "$(RUFF_VERSION)" || uv tool install --force --quiet ruff==$(RUFF_VERSION)

FRONTEND_STAMP=node_modules/.stamp
HTTP?=:2242
ANDROID_GRADLE=cd android && ./gradlew --no-daemon --quiet
# Kotlin is checked only when the branch changes it: the Gradle invocation is slow,
# and this keeps pre-push cheap in the common case.
KOTLIN_BASE?=$(shell git merge-base HEAD @{u} 2>/dev/null || git merge-base HEAD origin/main 2>/dev/null || echo HEAD)
ANDROID_BUILD_TASKS=:gomode:assembleDebug :halo-sdk:assembleDebug :caic-sdk:assemble :gomode-sdk:assemble :mcp-sdk:assemble :voicegateway-sdk:assemble
ANDROID_TEST_BUILD_TASKS=:gomode:assembleDebugAndroidTest :halo-sdk:assembleDebugAndroidTest
ANDROID_TEST_TASKS=:gomode:testDebugUnitTest :caic-sdk:test :gomode-sdk:test :mcp-sdk:test :voicegateway-sdk:test
ANDROID_COVERAGE_REPORT_TASKS=:gomode:createDebugUnitTestCoverageReport :halo-sdk:createDebugUnitTestCoverageReport
ANDROID_COVERAGE_TASKS=$(ANDROID_TEST_TASKS) $(ANDROID_COVERAGE_REPORT_TASKS)
ANDROID_LINT_TASKS=:gomode:detekt :halo-sdk:detekt :gomode:ktlintCheck :halo-sdk:ktlintCheck :gomode:lint :halo-sdk:lint

help:
	@echo "caic - Manage multiple coding agents"
	@echo ""
	@echo "Available targets:"
	@echo "  make benchmark              - Run Go and frontend benchmarks"
	@echo "  make check                  - Refresh generated files, build, lint, and test (non-Android)"
	@echo "  make test-all               - Run every non-smoke test and deterministic visual check"
	@echo "  make check-agent-logs       - Validate recent v2 task logs against genai wire DTOs"
	@echo "  make lint                   - Fix what is autofixable, then run lint-check"
	@echo "  make lint-check             - Check lint without writing (Go + frontend + Python + Kotlin + binaries + docs)"
	@echo "  make format                 - Apply the shared formatters (prettier, gofmt, ruff, shfmt, ktlint)"
	@echo "  make format-kotlin          - Apply ktlint to the Android and Halo Kotlin sources"
	@echo "  make format-check           - Verify formatting without writing"
	@echo "  make build                  - Build Go server (includes frontend build)"
	@echo "  make fake-dev               - Run the server with fake backend (no containers)"
	@echo "  make frontend-dev           - Run frontend dev server (http://localhost:5173)"
	@echo "  make frontend-e2e           - Run Playwright end-to-end tests"
	@echo "  make screenshots-check      - Verify deterministic frontend and Android screenshots"
	@echo "  make screenshots-check-frontend - Verify only the frontend screenshots (no emulator)"
	@echo "  make screenshots-check-android  - Verify only the Android screenshots (needs the emulator)"
	@echo "  make screenshots-generate-frontend - Render the frontend screenshots without comparing"
	@echo "  make screenshots-generate-android  - Render the Android screenshots without comparing"
	@echo "  make screenshots-update     - Explicitly update deterministic screenshot baselines"
	@echo "  make smoke                  - Run real runtime smoke test"
	@echo "  make smoke-voice            - Run local voice WebRTC smoke test"
	@echo "  make refresh-generated      - Regenerate API SDKs, AGENTS indexes, and backend architecture docs"
	@echo "  make android-check          - Run Android lint, build, unit tests, and coverage"
	@echo "  make android-e2e            - Start the emulator and run Android E2E tests"
	@echo "  make android-push-gomode    - Build, install, and start GoMode APK on connected device"
	@echo "  make android-start-emulator - Set up and start the headless Android emulator"
	@echo "  make android-stop-emulator  - Stop the running Android emulator"
	@echo "  make android-sdk            - Install required Android SDK packages"
	@echo "  make git-hooks              - Install git pre-commit hooks"
	@echo "  make upgrade                - Upgrade Go and pnpm dependencies"

$(FRONTEND_STAMP): pnpm-lock.yaml
	@pnpm install --frozen-lockfile --silent
	@touch $@

generate-sdks:
	@go generate ./...

refresh-generated: generate-sdks
	@./scripts/update_agents_file_index.py
	@./scripts/update_backend_architecture.py

frontend-build: $(FRONTEND_STAMP) generate-sdks
	@pnpm --silent build


# methodfilecheck (see .golangci.yml) is a golangci-lint module plugin, so the
# Go linting must run through the custom binary built from the published
# plugin module.
custom-gcl: .custom-gcl.yml
	@golangci-lint custom --version $(GOLANGCI_LINT_VERSION)

build: frontend-build
	@go install -trimpath -ldflags="-s -w -buildid=" ./backend/cmd/...

check: refresh-generated build lint-check test

test-all:
	@$(MAKE) check
	@$(MAKE) frontend-e2e
	@$(MAKE) android-check
	@$(MAKE) android-e2e
	@$(MAKE) screenshots-check

check-agent-logs:
	@go run ./backend/internal/cmd/check-agent-logs

benchmark: $(FRONTEND_STAMP)
	@go test ./... -run '^$$' -bench . -benchmem
	@pnpm --silent benchmark

fake-dev: frontend-build
	@./scripts/run-dev.py --http $(HTTP) --fake

test: $(FRONTEND_STAMP)
	@go test -cover ./...
	@pnpm --silent test:coverage
	@python3 scripts/run_python_tests.py

smoke:
	@go test -tags="smoke" -run TestSmoke -v -timeout 30m -coverprofile=coverage.out ./backend/cmd/caic/

smoke-voice:
	@go test -tags="smoke" -run TestSmokeVoiceRTCLocalAudio -v -timeout 15m ./gomode/voicegateway/voicertc/

coverage: $(FRONTEND_STAMP)
	@go test -coverprofile=coverage.out ./...
	@echo ""
	@echo "=== Go coverage ==="
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -html=coverage.out -o coverage.html
	@echo "  HTML report: coverage.html"
	@echo ""
	@echo "=== Frontend coverage ==="
	@pnpm --silent test:coverage

lint-check: tools lint-go lint-frontend lint-python lint-kotlin lint-binaries lint-docs

verify: format-check lint-check

lint-docs:
	@python3 scripts/update_agents_file_index.py --check
	@python3 scripts/update_backend_architecture.py --check

lint-go: tools custom-gcl
	@./custom-gcl run --show-stats=false ./...
	@# Compile-check build-tagged code (e.g. smoke tests) that golangci-lint skips.
	@python3 scripts/lint_build_tags.py

lint-frontend: $(FRONTEND_STAMP)
	@pnpm --silent typecheck
	@pnpm --silent lint:check
	@pnpm --silent lint:style
	@node scripts/lint_frontend_styles.mjs

# Apply and verify the shared formatters: prettier for the web and prose sources,
# gofmt and goimports through golangci-lint for Go, ruff format for the Python
# scripts, and shfmt for the shell scripts.
# Prettier skips whatever .prettierignore excludes (locks, generated code, testdata).
# The stylelint fixer runs last: its cascade-sensitive rewrites must not be undone by
# another formatter.
format: tools format-kotlin $(FRONTEND_STAMP)
	@pnpm --silent format
	@golangci-lint fmt
	@ruff format --quiet .
	@files=$$(git ls-files '*.sh' 'scripts/hooks/*'); [ -z "$$files" ] || shfmt -w $$files
	@pnpm --silent lint:style:fix

format-check: tools $(FRONTEND_STAMP)
	@pnpm --silent format:check
	@out=$$(golangci-lint fmt --diff); [ -z "$$out" ] || { echo 'Go files need formatting (gofmt, goimports):' >&2; echo "$$out" >&2; exit 1; }
	@ruff format --check --quiet .
	@files=$$(git ls-files '*.sh' 'scripts/hooks/*'); [ -z "$$files" ] || { out=$$(shfmt -l $$files); [ -z "$$out" ] || { echo 'Shell files need shfmt:' >&2; echo "$$out" >&2; exit 1; }; }

lint-python: tools
	@ruff check --quiet .

# Format the hand-written Kotlin modules. Generated SDK Kotlin is tool-owned and
# intentionally not wired to ktlint.
format-kotlin:
	@$(ANDROID_GRADLE) :gomode:ktlintFormat :halo-sdk:ktlintFormat

lint-kotlin:
	@base="$(KOTLIN_BASE)"; \
	changed=$$(git diff --name-only "$$base" -- '*.kt' '*.kts'; git ls-files --others --exclude-standard -- '*.kt' '*.kts'); \
	if [ -n "$$changed" ]; then \
		$(ANDROID_GRADLE) :gomode:ktlintCheck :halo-sdk:ktlintCheck; \
	fi

lint-binaries:
	@python3 scripts/lint_binaries.py

android-sdk:
	@python3 scripts/android_sdk.py check

android-check: android-sdk
	@$(ANDROID_GRADLE) $(ANDROID_LINT_TASKS) $(ANDROID_BUILD_TASKS) $(ANDROID_TEST_BUILD_TASKS) $(ANDROID_COVERAGE_TASKS)

android-setup-emulator:
	@python3 scripts/android_sdk.py setup-emulator

android-start-emulator: android-setup-emulator
	@python3 scripts/android_start_emulator.py

android-stop-emulator:
	@echo "Stopping emulator..."
	@(command -v adb >/dev/null 2>&1 && adb emu kill) || pkill -f emulator || true

android-push-gomode: android-check
	@devices=$$(adb devices | awk '/\tdevice$$/{print $$1}'); \
	[ -n "$$devices" ] || { echo "No devices connected"; exit 1; }; \
	for d in $$devices; do \
		(echo "Pushing to $$d..." && \
		 adb -s $$d install -r android/gomode/build/outputs/apk/debug/gomode-debug.apk && \
		 adb -s $$d shell am start -n com.fghbuild.gomode/.MainActivity && \
		 echo "Done: $$d") & \
	done; \
	wait

android-e2e: android-setup-emulator
	@python3 scripts/android_start_emulator.py --reuse-connected-device
	@python3 scripts/android_e2e.py

# Apply the autofixes, then report what is left to fix by hand.
lint: tools custom-gcl $(FRONTEND_STAMP)
	@./custom-gcl run --show-stats=false ./... --fix
	@pnpm --silent lint:fix
	@pnpm --silent lint:style:fix
	@ruff check --quiet --fix .
	@./scripts/update_agents_file_index.py
	@./scripts/update_backend_architecture.py
	@$(MAKE) --no-print-directory lint-check

git-hooks:
	@./scripts/install-git-hooks.sh
	@git config merge.ours.driver true
	@echo "✓ Git hooks installed"

frontend-dev: $(FRONTEND_STAMP)
	@pnpm --silent dev

playwright-browser: $(FRONTEND_STAMP)
	@pnpm --silent exec playwright install chromium

frontend-e2e: $(FRONTEND_STAMP) generate-sdks playwright-browser
	@pnpm --silent build
	@pnpm --silent exec playwright test --config e2e/playwright.config.ts

screenshots-check: $(FRONTEND_STAMP) generate-sdks playwright-browser android-setup-emulator
	@pnpm --silent build
	@python3 scripts/android_start_emulator.py --auto-reuse
	@python3 scripts/visual_screenshots.py check

# Platform-specific variants. Check compares against the tracked baselines, which
# encode the development container's font stack, so it is a maintainer check.
# Generate only renders, which is what CI runs so a stale generator cannot rot
# silently without pretending the baselines are portable.
screenshots-check-frontend: $(FRONTEND_STAMP) generate-sdks playwright-browser
	@pnpm --silent build
	@python3 scripts/visual_screenshots.py check --platform frontend

# The Android variant renders the committed frontend bundle the app hosts, so it
# needs no pnpm or SDK step.
screenshots-check-android: android-setup-emulator
	@python3 scripts/android_start_emulator.py --auto-reuse
	@python3 scripts/visual_screenshots.py check --platform android

screenshots-generate-frontend: $(FRONTEND_STAMP) generate-sdks playwright-browser
	@pnpm --silent build
	@python3 scripts/visual_screenshots.py generate --platform frontend

screenshots-generate-android: android-setup-emulator
	@python3 scripts/android_start_emulator.py --auto-reuse
	@python3 scripts/visual_screenshots.py generate --platform android

screenshots-update: $(FRONTEND_STAMP) generate-sdks playwright-browser android-setup-emulator
	@pnpm --silent build
	@python3 scripts/android_start_emulator.py --auto-reuse
	@python3 scripts/visual_screenshots.py update

upgrade:
	@go get -u ./... && go mod tidy
	@pnpm --silent update --latest
	@cd android && ./gradlew --no-daemon dependencyUpdates -Drevision=release
