# Build, test, lint, and development workflow targets for the full stack (Go backend, TypeScript frontend, Android).

.PHONY: help build dev fake-dev test smoke smoke-voice coverage lint lint-all lint-go lint-frontend lint-python lint-binaries lint-android lint-fix lint-docs types git-hooks frontend-build frontend-dev upgrade frontend-e2e android-build android-push android-test android-coverage android-e2e android-setup-emulator android-start-emulator android-stop-emulator

FRONTEND_STAMP=node_modules/.stamp
HTTP?=:2242

help:
	@echo "caic - Manage multiple coding agents"
	@echo ""
	@echo "Available targets:"
	@echo "  make build                  - Build Go server (includes frontend build)"
	@echo "  make dev                    - Run the server in development mode (go run)"
	@echo "  make fake-dev               - Run the server with fake backend (no containers)"
	@echo "  make frontend-build         - Build frontend assets (TypeScript → JavaScript)"
	@echo "  make test                   - Run unit tests"
	@echo "  make smoke                  - Run real runtime smoke test"
	@echo "  make smoke-voice            - Run local voice WebRTC smoke test"
	@echo "  make lint                   - Run linters (Go + frontend + Python + binaries + file index check)"
	@echo "  make lint-fix               - Fix linting issues automatically (includes updating file indexes)"
	@echo "  make git-hooks              - Install git pre-commit hooks"
	@echo "  make frontend-dev           - Run frontend dev server (http://localhost:5173)"
	@echo "  make android-build          - Build Android app (debug APK)"
	@echo "  make android-push           - Build, install, and start APK on connected device"
	@echo "  make android-test           - Run Android unit tests"
	@echo "  make android-coverage       - Run Android unit tests with JaCoCo coverage"
	@echo "  make android-e2e            - Run Android instrumented tests and generate screenshots"
	@echo "  make android-setup-emulator - Install SDK tools, emulator, system image, create AVD"
	@echo "  make android-start-emulator - Start the headless Android emulator"
	@echo "  make android-stop-emulator  - Stop the running Android emulator"
	@echo "  make frontend-e2e           - Run Playwright end-to-end tests"
	@echo "  make lint-android           - Run Android linters (detekt + lint)"
	@echo "  make upgrade                - Upgrade Go and pnpm dependencies"

$(FRONTEND_STAMP): pnpm-lock.yaml
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm install --frozen-lockfile --silent
	@touch $@

types:
	@go generate ./...

frontend-build: $(FRONTEND_STAMP) types
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm build

build: frontend-build
	@go install -trimpath -ldflags="-s -w -buildid=" ./backend/cmd/...

dev: frontend-build
	@./scripts/run-dev.py --http $(HTTP)

fake-dev: frontend-build
	@./scripts/run-dev.py --http $(HTTP) --fake

test: $(FRONTEND_STAMP)
	@go test -cover ./...
	@pnpm test
	@find . -name 'test_*.py' -exec python3 {} \;

smoke:
	@go test -tags="smoke" -run TestSmoke -v -timeout 15m ./backend/cmd/caic/

smoke-voice:
	@go test -tags="smoke" -run TestSmokeVoiceRTCLocalAudio -v -timeout 15m ./backend/internal/voicegateway/voicertc/

coverage: $(FRONTEND_STAMP)
	@go test -coverprofile=coverage.out ./...
	@echo ""
	@echo "=== Go coverage ==="
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -html=coverage.out -o coverage.html
	@echo "  HTML report: coverage.html"
	@echo ""
	@echo "=== Frontend coverage ==="
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm test:coverage

lint: lint-go lint-frontend lint-python lint-binaries lint-docs
lint-all: lint-go lint-frontend lint-python lint-binaries lint-android lint-docs

lint-docs:
	@python3 scripts/update_agents_file_index.py --check
	@python3 scripts/update_backend_architecture.py --check

lint-go:
	@which golangci-lint > /dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	@golangci-lint run ./...

lint-frontend: $(FRONTEND_STAMP)
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm lint
	@python3 scripts/lint_css_vars.py

lint-python:
	@ruff check .
	@ruff format --check .

lint-binaries:
	@python3 scripts/lint_binaries.py

lint-android:
	@cd android && ./gradlew --no-daemon detekt lint

android-build:
	@cd android && ./gradlew --no-daemon assembleDebug

android-setup-emulator:
	@python3 scripts/android_setup_emulator.py

android-start-emulator:
	@python3 scripts/android_start_emulator.py

android-stop-emulator:
	@echo "Stopping emulator..."
	@(command -v adb >/dev/null 2>&1 && adb emu kill) || pkill -f emulator || true

android-push: android-build
	@devices=$$(adb devices | awk '/\tdevice$$/{print $$1}'); \
	[ -n "$$devices" ] || { echo "No devices connected"; exit 1; }; \
	for d in $$devices; do \
		(echo "Pushing to $$d..." && \
		 adb -s $$d install -r android/caic/build/outputs/apk/debug/caic-debug.apk && \
		 adb -s $$d shell am start -n com.fghbuild.caic/.MainActivity && \
		 echo "Done: $$d") & \
	done; \
	wait

android-test:
	@cd android && ./gradlew --no-daemon test

android-coverage:
	@cd android && ./gradlew --no-daemon :caic:testDebugUnitTest :caic:createDebugUnitTestCoverageReport :halo-sdk:testDebugUnitTest :halo-sdk:createDebugUnitTestCoverageReport

android-e2e:
	@python3 scripts/android_e2e.py

lint-fix: $(FRONTEND_STAMP)
	@golangci-lint run ./... --fix || true
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm lint:fix
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
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm dev

frontend-e2e: $(FRONTEND_STAMP) types
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm build
	@pnpm exec playwright test --config e2e/playwright.config.ts

upgrade:
	@go get -u ./... && go mod tidy
	@pnpm update --latest
	@cd android && ./gradlew --no-daemon dependencyUpdates -Drevision=release
