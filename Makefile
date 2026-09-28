# Build, verify, test, and development workflow targets for the Go backend and TypeScript frontend.

.DEFAULT_GOAL := help
.PHONY: help benchmark build check-agent-logs coverage custom-gcl fake-dev fix generate-sdks git-hooks frontend-build frontend-dev playwright-browser refresh-generated test test-e2e test-smoke tools upgrade verify screenshots-check screenshots-generate-frontend screenshots-update

# Ruff is installed separately; Go tool versions are declared in go.mod.
RUFF_VERSION=0.16.8

# Prepend uv's tool directory so a recipe can run Ruff after installing it.
UV_BIN := $(if $(shell command -v uv 2>/dev/null),$(shell uv tool dir --bin 2>/dev/null))
export PATH := $(if $(UV_BIN),$(UV_BIN):)$(PATH)

tools:
	@command -v uv > /dev/null 2>&1 || { echo 'uv is required to install the Python tools; see https://docs.astral.sh/uv/' >&2; exit 1; }
	@ruff --version 2>/dev/null | grep -Fqw "$(RUFF_VERSION)" || uv tool install --force --quiet ruff==$(RUFF_VERSION)

FRONTEND_STAMP=node_modules/.modules.yaml
HTTP?=:2242

# Static checks for verify, grouped into independent lanes run concurrently by
# scripts/run-concurrently.sh. Each is read-only; fix applies their autofixes.
#
# The verify recipe passes these single-quoted through two shell layers, so a
# lane variable must not contain a single quote; use double quotes inside.
#
# The gofmt and goimports formatters are checked by custom-gcl run itself
# (formatters section of .golangci.yml) with its warm analysis cache; a separate
# `golangci-lint fmt --diff` pass would re-typecheck the whole tree without that
# cache. Caveat: when another linter fails on the same file, run reports the
# lint error only, so a formatting problem there surfaces on the next verify
# after the lint fix. fix applies the formatters through `golangci-lint fmt`.
# methodfilecheck (see .golangci.yml) is a golangci-lint module plugin, so Go
# linting must run through the custom binary built from the published plugin
# module; the plain binary is fine for `fmt`.
VERIFY_GO = ./custom-gcl run --show-stats=false ./...
VERIFY_GOBUILD = python3 scripts/lint_build_tags.py
VERIFY_JS = pnpm exec prettier --check --log-level warn --cache --cache-location node_modules/.cache/prettier/.prettier-cache --cache-strategy content . && pnpm exec stylelint "frontend/src/**/*.css" && node scripts/lint_frontend_styles.mjs
VERIFY_TS = pnpm --silent typecheck
VERIFY_ESLINT = pnpm exec eslint --cache --cache-location node_modules/.cache/eslint/ --cache-strategy content .
VERIFY_PY = ruff format --check --quiet . && ruff check --quiet .
VERIFY_SH = files=$$(git ls-files "*.sh" "scripts/hooks/*"); [ -z "$$files" ] || go tool shfmt -l $$files
VERIFY_MISC = python3 scripts/lint_binaries.py && python3 scripts/update_agents_file_index.py --check && python3 scripts/update_backend_architecture.py --check

help:
	@echo 'caic - Manage multiple coding agents'
	@echo ''
	@echo 'Available targets:'
	@printf '  %-34s - %s\n' 'make fix' 'Apply every autofix, then refresh generated indexes'
	@printf '  %-34s - %s\n' 'make verify' 'Fast static gate: lint, formatting, generated docs (pre-push gate)'
	@printf '  %-34s - %s\n' 'make test' 'Run unit tests (Go, frontend, Python)'
	@printf '  %-34s - %s\n' 'make test-e2e' 'Run Playwright end-to-end tests (slow, needs a frontend build)'
	@printf '  %-34s - %s\n' 'make test-smoke' 'Run real runtime smoke test (slow, needs md containers)'
	@printf '  %-34s - %s\n' 'make test-smoke-voice' 'Run local voice WebRTC smoke test (slow, needs audio setup)'
	@printf '  %-34s - %s\n' 'make benchmark' 'Run Go and frontend benchmarks'
	@printf '  %-34s - %s\n' 'make check-agent-logs' 'Validate recent v2 task logs against genai wire DTOs'
	@printf '  %-34s - %s\n' 'make build' 'Build Go server (includes frontend build)'
	@printf '  %-34s - %s\n' 'make fake-dev' 'Run the server with fake backend (no containers)'
	@printf '  %-34s - %s\n' 'make frontend-dev' 'Run frontend dev server (http://localhost:5173)'
	@printf '  %-34s - %s\n' 'make refresh-generated' 'Regenerate API SDKs, AGENTS indexes, and backend architecture docs'
	@printf '  %-34s - %s\n' 'make screenshots-check' 'Verify deterministic frontend screenshots'
	@printf '  %-34s - %s\n' 'make screenshots-generate-frontend' 'Render the frontend screenshots without comparing'
	@printf '  %-34s - %s\n' 'make screenshots-update' 'Explicitly update deterministic screenshot baselines'
	@printf '  %-34s - %s\n' 'make git-hooks' 'Install git pre-commit hooks'
	@printf '  %-34s - %s\n' 'make upgrade' 'Upgrade Go and pnpm dependencies'

$(FRONTEND_STAMP): pnpm-lock.yaml
	@echo 'Installing frontend dependencies (one-off after a lockfile change)...'
	@pnpm install --frozen-lockfile --silent
	@touch $@

generate-sdks:
	@go generate ./...

refresh-generated: generate-sdks
	@./scripts/update_agents_file_index.py
	@./scripts/update_backend_architecture.py

frontend-build: $(FRONTEND_STAMP) generate-sdks
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

# The one static gate. Runs every check-only lane concurrently; the read-only
# counterpart of fix and the pre-push gate. Independent of test.
verify: tools custom-gcl $(FRONTEND_STAMP)
	@go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
	@./scripts/run-concurrently.sh go,buildtags,js,ts,eslint,python,shell,misc '$(VERIFY_GO)' '$(VERIFY_GOBUILD)' '$(VERIFY_JS)' '$(VERIFY_TS)' '$(VERIFY_ESLINT)' '$(VERIFY_PY)' '$(VERIFY_SH)' '$(VERIFY_MISC)'

# Apply every autofix, then refresh the generated file index and architecture
# diagram. Order matters: the stylelint fixer runs last because its
# cascade-sensitive rewrites must not be undone by another formatter, and the
# index refresh runs after fixes so the index matches the fixed tree. Does not
# re-check; run verify for that.
fix: tools custom-gcl $(FRONTEND_STAMP)
	@./custom-gcl run --show-stats=false ./... --fix
	@go tool golangci-lint fmt
	@pnpm exec eslint . --fix
	@pnpm exec prettier --write --log-level warn .
	@ruff check --quiet --fix .
	@ruff format --quiet .
	@files=$$(git ls-files '*.sh' 'scripts/hooks/*'); [ -z "$$files" ] || go tool shfmt -w $$files
	@pnpm exec stylelint --fix "frontend/src/**/*.css"
	@./scripts/update_agents_file_index.py
	@./scripts/update_backend_architecture.py

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

# End-to-end tests run against the fake backend (see e2e/playwright.config.ts):
# slow, needs a frontend build and the Playwright chromium build, and shares
# backend/frontend/dist with build targets, so never run it concurrently with
# them. CI runs it; verify and test do not.
test-e2e: $(FRONTEND_STAMP) generate-sdks playwright-browser
	@pnpm --silent build
	@pnpm --silent exec playwright test --config e2e/playwright.config.ts; \
	status=$$?; \
	if [ $$status -ne 0 ]; then \
	  echo ""; echo "=== Fake server log (test-results/e2e-server.log), last 100 lines: ==="; \
	  tail -n 100 test-results/e2e-server.log 2>/dev/null; \
	fi; \
	exit $$status

# Real runtime smoke tests exercise the md container path and cannot run
# without that runtime (see the smoke build tag sources for what they need).
test-smoke:
	@go test -tags="smoke" -run TestSmoke -v -timeout 30m -coverprofile=coverage.out ./backend/cmd/caic/

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

git-hooks:
	@./scripts/install-git-hooks.sh
	@git config merge.ours.driver true
	@echo "✓ Git hooks installed"

frontend-dev: $(FRONTEND_STAMP)
	@pnpm --silent dev

playwright-browser: $(FRONTEND_STAMP)
	@pnpm --silent exec playwright install chromium

# Slow, maintainer-only: renders frontend visuals twice and
# compares decoded pixels against tracked baselines that encode the
# development container's font stack.
screenshots-check: $(FRONTEND_STAMP) generate-sdks playwright-browser
	@pnpm --silent build
	@python3 scripts/visual_screenshots.py check --rebuilt

# Platform-specific variants. Check compares against the tracked baselines, which
# encode the development container's font stack, so it is a maintainer check.
# Generate only renders, which is what CI runs so a stale generator cannot rot
# silently without pretending the baselines are portable. The renderer refuses
# to compare a bundle that predates uncommitted frontend inputs, so it would
# otherwise catch a bare script invocation that forgot to build; these targets
# build immediately above and declare that with --rebuilt.
screenshots-generate-frontend: $(FRONTEND_STAMP) generate-sdks playwright-browser
	@pnpm --silent build
	@python3 scripts/visual_screenshots.py generate --rebuilt

screenshots-update: $(FRONTEND_STAMP) generate-sdks playwright-browser
	@pnpm --silent build
	@python3 scripts/visual_screenshots.py update --rebuilt

upgrade:
	@go get -u ./... && go mod tidy
	@pnpm --silent update --latest
