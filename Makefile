# Build, benchmark, test, lint, and development workflow targets for the full stack (Go backend, TypeScript frontend, Android).

.PHONY: help benchmark build check check-agent-logs fake-dev test smoke smoke-voice coverage lint lint-go lint-frontend lint-python lint-binaries lint-fix lint-docs refresh-generated generate-sdks git-hooks frontend-build frontend-dev upgrade frontend-e2e android-sdk android-check android-push-gomode android-e2e android-setup-emulator android-start-emulator android-stop-emulator

FRONTEND_STAMP=node_modules/.stamp
HTTP?=:2242
ANDROID_GRADLE=cd android && ./gradlew --no-daemon
export NPM_CONFIG_AUDIT=false
export NPM_CONFIG_FUND=false
ANDROID_BUILD_TASKS=:gomode:assembleDebug :halo-sdk:assembleDebug :caic-sdk:assemble :gomode-sdk:assemble :mcp-sdk:assemble :voicegateway-sdk:assemble
ANDROID_TEST_BUILD_TASKS=:gomode:assembleDebugAndroidTest :halo-sdk:assembleDebugAndroidTest
ANDROID_TEST_TASKS=:gomode:testDebugUnitTest :caic-sdk:test :gomode-sdk:test :mcp-sdk:test :voicegateway-sdk:test
ANDROID_COVERAGE_REPORT_TASKS=:gomode:createDebugUnitTestCoverageReport :halo-sdk:createDebugUnitTestCoverageReport
ANDROID_COVERAGE_TASKS=$(ANDROID_TEST_TASKS) $(ANDROID_COVERAGE_REPORT_TASKS)
ANDROID_LINT_TASKS=:gomode:detekt :halo-sdk:detekt :gomode:lint :halo-sdk:lint

help:
	@echo "caic - Manage multiple coding agents"
	@echo ""
	@echo "Available targets:"
	@echo "  make benchmark              - Run Go benchmarks"
	@echo "  make check                  - Refresh generated files, build, lint, and test (non-Android)"
	@echo "  make check-agent-logs       - Validate recent v2 task logs against genai wire DTOs"
	@echo "  make lint-fix               - Fix linting issues (Go + frontend + Python + binaries + file indexes)"
	@echo "  make build                  - Build Go server (includes frontend build)"
	@echo "  make fake-dev               - Run the server with fake backend (no containers)"
	@echo "  make frontend-dev           - Run frontend dev server (http://localhost:5173)"
	@echo "  make frontend-e2e           - Run Playwright end-to-end tests"
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
	@pnpm build

build: frontend-build
	@go install -trimpath -ldflags="-s -w -buildid=" ./backend/cmd/...

check: refresh-generated build lint test

check-agent-logs:
	@go run ./backend/internal/cmd/check-agent-logs

benchmark:
	@go test ./... -run '^$$' -bench . -benchmem

fake-dev: frontend-build
	@./scripts/run-dev.py --http $(HTTP) --fake

test: $(FRONTEND_STAMP)
	@go test -cover ./...
	@pnpm test:coverage
	@find . -name 'test_*.py' -exec python3 {} \;

smoke:
	@go test -tags="smoke" -run TestSmoke -v -timeout 15m -coverprofile=coverage.out ./backend/cmd/caic/

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
	@pnpm test:coverage

lint: lint-go lint-frontend lint-python lint-binaries lint-docs

lint-docs:
	@python3 scripts/update_agents_file_index.py --check
	@python3 scripts/update_backend_architecture.py --check

lint-go:
	@which golangci-lint > /dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	@golangci-lint run ./...
	@# Compile-check build-tagged code (e.g. smoke tests) that golangci-lint skips.
	@go vet -tags=smoke ./...

lint-frontend: $(FRONTEND_STAMP)
	@pnpm typecheck
	@pnpm lint
	@python3 scripts/lint_css_vars.py

lint-python:
	@ruff check .
	@ruff format --check .

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
	@python3 scripts/android_start_emulator.py --auto-reuse
	@python3 scripts/android_e2e.py

lint-fix: $(FRONTEND_STAMP)
	@golangci-lint run ./... --fix
	@pnpm lint:fix
	@ruff check --fix .
	@ruff format .
	@./scripts/update_agents_file_index.py
	@./scripts/update_backend_architecture.py

git-hooks:
	@mkdir -p .git/hooks
	@cp ./scripts/pre-commit .git/hooks/pre-commit
	@cp ./scripts/pre-push .git/hooks/pre-push
	@git config merge.ours.driver true
	@echo "✓ Git hooks installed"

frontend-dev: $(FRONTEND_STAMP)
	@pnpm dev

frontend-e2e: $(FRONTEND_STAMP) generate-sdks
	@pnpm build
	@pnpm exec playwright test --config e2e/playwright.config.ts

upgrade:
	@go get -u ./... && go mod tidy
	@pnpm update --latest
	@cd android && ./gradlew --no-daemon dependencyUpdates -Drevision=release
