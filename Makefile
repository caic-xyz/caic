.PHONY: help build dev test coverage lint lint-go lint-frontend lint-python lint-fix docs types git-hooks frontend-dev upgrade e2e

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
	@echo "  make lint           - Run linters (Go + frontend)"
	@echo "  make lint-fix       - Fix linting issues automatically"
	@echo "  make git-hooks      - Install git pre-commit hooks"
	@echo "  make frontend-dev   - Run frontend dev server (http://localhost:5173)"
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

test:
	@go test -cover ./...
	@find . -name 'test_*.py' -exec python3 {} \;

coverage:
	@go test -coverprofile=coverage.out ./...

lint: lint-go lint-frontend lint-python

lint-go:
	@which golangci-lint > /dev/null || go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	@golangci-lint run ./...

lint-frontend: $(FRONTEND_STAMP)
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm lint

lint-python:
	@ruff check .
	@ruff format --check .

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

e2e: $(FRONTEND_STAMP) types
	@NPM_CONFIG_AUDIT=false NPM_CONFIG_FUND=false pnpm build
	@pnpm exec playwright test --config e2e/playwright.config.ts

upgrade:
	@go get -u ./... && go mod tidy
	@pnpm update --latest
