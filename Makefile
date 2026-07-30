.PHONY: help bootstrap mod-verify fmt fmt-check lint vuln ci-lint test test-integration build migrate-validate migrate-up migrate-down migrate-version compose-up compose-down compose-logs run-gateway run-control-plane run-metering run-mock-provider check

ifneq (,$(wildcard .env))
include .env
export
endif

help:
	@echo "Targets: bootstrap mod-verify fmt fmt-check lint vuln ci-lint test test-integration build migrate-validate migrate-up migrate-down migrate-version compose-up compose-down run-* check"

bootstrap:
	go mod download

mod-verify:
	go mod verify

fmt:
	go tool golangci-lint fmt

fmt-check:
	@output="$$(go tool golangci-lint fmt --diff)"; if [ -n "$$output" ]; then echo "$$output"; exit 1; fi

lint:
	go vet ./...
	go tool golangci-lint config verify
	go tool golangci-lint run --timeout=5m ./...
	go tool golangci-lint run --build-tags=integration --timeout=5m ./tests/integration/...

vuln:
	go tool govulncheck ./...

ci-lint:
	go tool actionlint .github/workflows/ci.yml

test:
	go test -race -count=1 ./...

test-integration:
	go test -race -count=1 -tags=integration -timeout=2m ./tests/integration/...

build:
	go build ./...

migrate-validate:
	go run ./cmd/migrate validate --path migrations

migrate-up:
	go run ./cmd/migrate up --path migrations

migrate-down:
	go run ./cmd/migrate down --path migrations --steps 1 --confirm-development

migrate-version:
	go run ./cmd/migrate version --path migrations

compose-up:
	docker compose --env-file .env -f deploy/compose/compose.yaml up -d

compose-down:
	docker compose --env-file .env -f deploy/compose/compose.yaml down

compose-logs:
	docker compose --env-file .env -f deploy/compose/compose.yaml logs -f

run-gateway:
	go run ./cmd/gateway

run-control-plane:
	go run ./cmd/control-plane

run-metering:
	go run ./cmd/metering-worker

run-mock-provider:
	go run ./cmd/mock-provider

check: mod-verify fmt-check lint test build vuln migrate-validate ci-lint
