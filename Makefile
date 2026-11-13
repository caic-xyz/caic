# Build, verify, test, and development workflow targets for the Go backend and TypeScript frontend.

.DEFAULT_GOAL := help
.PHONY: help benchmark build coverage custom-gcl fix generate-sdks git-hooks frontend-build frontend-deps playwright-browser screenshots-update test test-e2e test-smoke upgrade verify

FRONTEND_TEST = pnpm exec tsx --test e2e/browser.test.ts e2e/bundle.test.ts && pnpm exec node --test scripts/css_module_types.test.mjs && NODE_OPTIONS=--no-experimental-webstorage pnpm exec tsx --test --test-concurrency=4 --conditions=browser --test-force-exit --test-reporter=./scripts/quiet-test-reporter.mjs --import ./frontend/tests/setup-dom.ts
FRONTEND_TEST_FILES = "frontend/src/**/*.test.ts" "frontend/src/**/*.test.tsx"

help:
	@echo 'caic - Manage multiple coding agents'
	@echo ''
	@echo 'Available targets:'
	@printf '  %-36s - %s\n' 'make fix' 'Apply every autofix and regenerate SDKs, indexes, and architecture docs'
	@printf '  %-36s - %s\n' 'make verify' 'Fast static gate: lint, formatting, generated docs (pre-push gate)'
	@printf '  %-36s - %s\n' 'make test' 'Run unit tests (Go, frontend, Python)'
	@printf '  %-36s - %s\n' 'make test-e2e [SPEC=...] [ROUNDS=N]' 'Run Playwright end-to-end tests, or one spec, N times (slow)'
	@printf '  %-36s - %s\n' 'make test-smoke' 'Run real runtime smoke test (slow, needs md containers)'
	@printf '  %-36s - %s\n' 'make benchmark' 'Run Go and frontend benchmarks'
	@printf '  %-36s - %s\n' 'make build' 'Build Go server (includes frontend build)'
	@printf '  %-36s - %s\n' 'make screenshots-update' 'Render twice and publish the documentation screenshots'
	@printf '  %-36s - %s\n' 'make git-hooks' 'Install git pre-commit hooks'
	@printf '  %-36s - %s\n' 'make upgrade' 'Upgrade Go and pnpm dependencies'

# Reconcile dependencies once per Make invocation before checks start. Warm
# frozen installs reuse pnpm's caches; missing packages require downloads.
frontend-deps:
	@pnpm install --frozen-lockfile --silent

generate-sdks:
	@go generate ./...

frontend-build: frontend-deps generate-sdks
	@pnpm --silent build

# The custom-gcl binary is not byte-reproducible (golangci-lint custom builds
# in a random temp directory and stamps VCS metadata), so staleness is tracked
# by hashing the build inputs instead of comparing mtimes: the plugin config
# plus the pinned golangci-lint version and the Go toolchain. A branch switch
# that recreates .custom-gcl.yml with a fresh timestamp must not trigger a
# rebuild; a version or config change must.
.PHONY: custom-gcl
custom-gcl:
	@version=$$(go list -m -f '{{.Version}}' github.com/golangci/golangci-lint/v2) || exit 1; \
	want=$$({ sha256sum .custom-gcl.yml | cut -d" " -f1; echo "$$version"; go env GOVERSION; } | sha256sum | cut -d" " -f1); \
	if [ -x custom-gcl ] && [ "$$want" = "$$(cat .custom-gcl.sha 2>/dev/null)" ]; then exit 0; fi; \
	echo 'Building custom-gcl with the methodfilecheck plugin (one-off; runs when the config, golangci-lint version, or Go toolchain changes)...'; \
	go tool golangci-lint custom --version "$$version" && echo "$$want" > .custom-gcl.sha

build: frontend-build
	@go install -trimpath -ldflags="-s -w -buildid=" ./backend/cmd/...

verify: custom-gcl frontend-deps git-hooks
	@./scripts/run-concurrently.sh \
		sdk,workflows,go,js,ts,eslint,python,shell,misc \
		'cd ./backend/internal/server/api/v1 && go run ../../../cmd/gen-api-sdk -check' \
		'go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12' \
		'./custom-gcl run --show-stats=false ./... && go mod tidy -diff' \
		'pnpm exec prettier --check --log-level warn --cache --cache-location node_modules/.cache/prettier/.prettier-cache --cache-strategy content . && pnpm exec stylelint "frontend/src/**/*.css" && pnpm exec node scripts/lint_frontend_styles.mjs' \
		'pnpm exec node scripts/css_module_types.mjs --source-path frontend/src && pnpm exec tsc --noEmit --incremental --tsBuildInfoFile node_modules/.cache/tsc/root.tsbuildinfo -p tsconfig.json && pnpm exec tsc --noEmit --incremental --tsBuildInfoFile node_modules/.cache/tsc/e2e.tsbuildinfo -p e2e/tsconfig.json' \
		'pnpm exec eslint --cache --cache-location node_modules/.cache/eslint/ --cache-strategy content .' \
		'uv run --locked ruff format --check --quiet . && uv run --locked ruff check --quiet .' \
		'files=$$(git ls-files "*.sh" "scripts/hooks/*"); [ -z "$$files" ] || go tool shfmt -d $$files' \
		'uv run --locked python scripts/lint_binaries.py && uv run --locked python scripts/update_agents_file_index.py --check && uv run --locked python scripts/update_backend_architecture.py --check'

# Regenerate the SDKs, apply every autofix, then refresh the generated file
# index and architecture diagram. Order matters: the stylelint fixer runs last because its
# cascade-sensitive rewrites must not be undone by another formatter, and the
# index refresh runs after fixes so the index matches the fixed tree. Does not
# re-check; run verify for that.
fix: custom-gcl frontend-deps generate-sdks
	@./custom-gcl run --show-stats=false ./... --fix
	@go tool golangci-lint fmt
	@pnpm exec stylelint --fix "frontend/src/**/*.css"
	@pnpm exec node scripts/css_module_types.mjs --source-path frontend/src --write
	@pnpm exec eslint . --fix
	@pnpm exec prettier --write --log-level warn .
	@uv run --locked ruff check --quiet --fix .
	@uv run --locked ruff format . >/dev/null
	@files=$$(git ls-files '*.sh' 'scripts/hooks/*'); [ -z "$$files" ] || go tool shfmt -w $$files
	@uv run --locked python scripts/update_agents_file_index.py
	@uv run --locked python scripts/update_backend_architecture.py

benchmark: frontend-deps
	@go test ./... -run '^$$' -bench . -benchmem
	@pnpm exec tsx scripts/bench.ts 'frontend/src/**/*.bench.ts'

# Independent Go, frontend, and Python suites run after dependency installation.
test: frontend-deps
	@./scripts/run-concurrently.sh \
		go,frontend,python \
		'go test -cover ./...' \
		'$(FRONTEND_TEST) --experimental-test-coverage $(FRONTEND_TEST_FILES)' \
		'uv run --locked python scripts/run_python_tests.py'

# End-to-end tests run against the fake backend (see e2e/playwright.config.ts):
# slow, needs a frontend build and the Playwright chromium build, and shares
# backend/frontend/dist with build targets, so never run it concurrently with
# them. CI runs it; verify and test do not. SPEC limits the run to one spec
# and may carry Playwright options. ROUNDS repeats the run until the first
# failure; Playwright starts a fresh fake backend per round, so in-memory fake
# state (device-code limits, fixtures) resets, unlike --repeat-each.
ROUNDS ?= 1
test-e2e: frontend-deps generate-sdks playwright-browser
	@case "$(ROUNDS)" in \
	  ''|*[!0-9]*|0) echo "ROUNDS must be a positive integer, got '$(ROUNDS)'"; exit 2 ;; \
	esac
	@pnpm --silent build
	@status=0; \
	for round in $$(seq 1 $(ROUNDS)); do \
	  [ $(ROUNDS) -eq 1 ] || echo "=== e2e round $$round/$(ROUNDS)"; \
	  pnpm --silent exec playwright test --config e2e/playwright.config.ts $(SPEC) || { status=$$?; break; }; \
	done; \
	if [ $$status -ne 0 ]; then \
	  echo ""; echo "=== Failed trace summary ==="; \
	  uv run --locked python scripts/summarize_playwright_trace.py test-results; \
	  echo ""; echo "=== Fake server log (test-results/e2e-server.log), last 100 lines: ==="; \
	  tail -n 100 test-results/e2e-server.log 2>/dev/null; \
	  auth_port=$${CAIC_E2E_AUTH_PORT:-$$(( $${CAIC_E2E_PORT:-8090} + 1 ))}; \
	  echo "=== Auth server log (test-results/e2e-auth-server-$$auth_port.log), last 100 lines: ==="; \
	  tail -n 100 "test-results/e2e-auth-server-$$auth_port.log" 2>/dev/null; \
	fi; \
	exit $$status

# Real runtime smoke tests exercise the md container path and cannot run
# without that runtime (see the smoke build tag sources for what they need).
test-smoke:
	@go test -tags="smoke" -run TestSmoke -v -timeout 30m -coverprofile=coverage.out ./backend/cmd/caic/

coverage: frontend-deps
	@go test -coverprofile=coverage.out ./...
	@echo ""
	@echo "=== Go coverage ==="
	@go tool cover -func=coverage.out | tail -1
	@go tool cover -html=coverage.out -o coverage.html
	@echo "  HTML report: coverage.html"
	@echo ""
	@echo "=== Frontend coverage ==="
	@$(FRONTEND_TEST) --experimental-test-coverage $(FRONTEND_TEST_FILES)

git-hooks:
	@./scripts/install-git-hooks.sh
	@git config merge.ours.driver true
	@hooks=$$(git config --local --get core.hooksPath); \
	  [ "$$hooks" = "scripts/hooks" ] || { \
	    echo "core.hooksPath is '$$hooks', want scripts/hooks; run make git-hooks" >&2; exit 1; \
	  }

# Local behavioral tests reuse installed Chrome; CI and visuals use pinned
# Chromium. Download only its headless shell when the shared detector needs it.
playwright-browser: frontend-deps
	@pnpm --silent exec tsx e2e/browser.ts --install

# Browser setup and screenshot rendering must both select the bundled shell.
# The tracked images encode the development container's font stack, so publish
# them from that container.
screenshots-update: export CAIC_E2E_VISUALS := 1
screenshots-update: frontend-deps generate-sdks playwright-browser
	@pnpm --silent build
	@uv run --locked python scripts/visual_screenshots.py

upgrade:
	@go get -u ./... && go mod tidy
	@pnpm --silent update --latest
