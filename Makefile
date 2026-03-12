.PHONY: help build dev test coverage lint lint-all lint-go lint-frontend lint-python lint-binaries lint-android lint-fix docs types git-hooks frontend-dev upgrade frontend-e2e android-build android-push android-test android-e2e android-setup-emulator android-start-emulator android-stop-emulator

FRONTEND_STAMP=node_modules/.stamp
HTTP?=:8080

help:
	@echo "caic - Manage multiple coding agents"
	@echo ""
	@echo "Available targets:"
	@echo "  make build          - Build Go server (auto-generates frontend)"
	@echo "  make dev            - Run the server in development mode"
	@echo "  make test           - Run unit tests"
	@echo "  make docs           - Update AGENTS.md file indexes"
	@echo "  make lint           - Run linters (Go + frontend + Python + binaries)"
	@echo "  make lint-fix       - Fix linting issues automatically"
	@echo "  make git-hooks      - Install git pre-commit hooks"
	@echo "  make frontend-dev   - Run frontend dev server (http://localhost:5173)"
	@echo "  make android-build  - Build Android app (debug APK)"
	@echo "  make android-push   - Build, install, and start APK on connected device"
	@echo "  make android-test   - Run Android unit tests"
	@echo "  make android-e2e    - Run Android instrumented tests"
	@echo "  make android-setup-emulator - Install emulator image and create AVD"
	@echo "  make android-start-emulator - Start the headless Android emulator"
	@echo "  make android-stop-emulator  - Stop the running Android emulator"
	@echo "  make frontend-e2e   - Run Playwright end-to-end tests"
	@echo "  make lint-android   - Run Android linters (detekt + lint)"
	@echo "  make upgrade        - Upgrade Go and pnpm dependencies"

	@echo "  make upgrade        - Upgrade Go and pnpm dependencies"

$(FRONTEND_STAMP): pnpm-lock.yaml
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm install --frozen-lockfile --silent
	@touch $@

types:
	@go generate ./...

build: $(FRONTEND_STAMP) types docs
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm build
	@go install -trimpath -ldflags="-s -w -buildid=" ./backend/cmd/...

docs:
	@./scripts/update_agents_file_index.py

dev: build
	@caic -http $(HTTP)

test: $(FRONTEND_STAMP)
	@go test -cover ./...
	@pnpm test
	@find . -name 'test_*.py' -exec python3 {} \;

coverage:
	@go test -coverprofile=coverage.out ./...

lint: lint-go lint-frontend lint-python lint-binaries
lint-all: lint-go lint-frontend lint-python lint-binaries lint-android

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
	@yes | sdkmanager --install "system-images;android-36;google_apis;x86_64"
	@echo "no" | avdmanager create avd -n caic_test -k "system-images;android-36;google_apis;x86_64" -d "pixel_6" --force

android-start-emulator:
	@echo "Starting emulator caic_test..."
	@$${ANDROID_HOME}/emulator/emulator -avd caic_test \
		-no-window -no-audio -gpu swiftshader_indirect -no-boot-anim -wipe-data > /dev/null 2>&1 & \
		adb wait-for-device && \
		echo "Emulator is ready."

android-stop-emulator:
	@echo "Stopping emulator..."
	@adb emu kill || pkill -f emulator || true

android-push: android-build
	@devices=$$(adb devices | awk '/\tdevice$$/{print $$1}'); \
	[ -n "$$devices" ] || { echo "No devices connected"; exit 1; }; \
	for d in $$devices; do \
		(echo "Pushing to $$d..." && \
		 adb -s $$d install -r android/app/build/outputs/apk/debug/app-debug.apk && \
		 adb -s $$d shell am start -n com.fghbuild.caic/.MainActivity && \
		 echo "Done: $$d") & \
	done; \
	wait

android-test:
	@cd android && ./gradlew --no-daemon test

android-e2e:
	@cd android && ./gradlew --no-daemon connectedAndroidTest

lint-fix: $(FRONTEND_STAMP)
	@golangci-lint run ./... --fix || true
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm lint:fix
	@ruff check --fix .
	@ruff format .

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
