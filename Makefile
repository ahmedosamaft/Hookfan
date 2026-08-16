# Go commands run against the host toolchain when one is installed, and fall
# back to the same image the Dockerfile builds with otherwise — so a checkout
# without Go still builds and tests identically, just slower.
GO_IMAGE := golang:1.25-alpine
GOCACHE_VOL := hookfan-gocache
GOMOD_VOL := hookfan-gomod

# Look for go on PATH, then in the default install locations.
HOST_GO := $(shell command -v go 2>/dev/null || \
	ls $(HOME)/.local/go/bin/go /usr/local/go/bin/go 2>/dev/null | head -1)

ifeq ($(HOST_GO),)
GORUN := docker run --rm \
	-v "$(CURDIR)":/src -w /src \
	-v $(GOCACHE_VOL):/root/.cache/go-build \
	-v $(GOMOD_VOL):/go/pkg/mod \
	-e CGO_ENABLED=0 \
	$(GO_IMAGE)
GO := $(GORUN) go
GOFMT := $(GORUN) gofmt
else
GO := CGO_ENABLED=0 $(HOST_GO)
GOFMT := $(dir $(HOST_GO))gofmt
endif

## toolchain — report which Go is in use
toolchain:
ifeq ($(HOST_GO),)
	@echo "using containerised Go ($(GO_IMAGE)); install Go locally for faster builds"
else
	@echo "using host Go: $(HOST_GO) ($$($(HOST_GO) version))"
endif

.PHONY: go toolchain tidy fmt vet test build up down logs psql clean \
	migrate-up migrate-down migrate-status migrate-create test-integration \
	ui-install ui-dev ui-build ui-check demo demo-clean

## go ARGS="..." — run an arbitrary go command
go:
	$(GO) $(ARGS)

tidy:
	$(GO) mod tidy

fmt:
	$(GOFMT) -l -w ./cmd ./internal

vet:
	$(GO) vet ./...

## test — unit tests only; database-backed tests skip themselves
test:
	$(GO) test ./... -count=1

## test-integration — spins up a throwaway Postgres, runs every test, tears it down
TEST_DB_NAME := hookfan-test-db
TEST_NET := hookfan-test-net
TEST_DB_PORT := 55432

ifeq ($(HOST_GO),)
# Containerised Go joins the test network and reaches Postgres by name.
TEST_DSN := postgres://hookfan:hookfan@$(TEST_DB_NAME):5432/hookfan_test?sslmode=disable
TEST_RUN := docker run --rm --network $(TEST_NET) \
	-v "$(CURDIR)":/src -w /src \
	-v $(GOCACHE_VOL):/root/.cache/go-build \
	-v $(GOMOD_VOL):/go/pkg/mod \
	-e CGO_ENABLED=0 \
	-e TEST_DATABASE_URL="postgres://hookfan:hookfan@$(TEST_DB_NAME):5432/hookfan_test?sslmode=disable" \
	$(GO_IMAGE) go
else
# Host Go reaches it through a published port instead.
TEST_RUN := TEST_DATABASE_URL="postgres://hookfan:hookfan@localhost:$(TEST_DB_PORT)/hookfan_test?sslmode=disable" \
	CGO_ENABLED=0 $(HOST_GO)
endif

test-integration:
	@docker network create $(TEST_NET) >/dev/null 2>&1 || true
	@docker rm -f $(TEST_DB_NAME) >/dev/null 2>&1 || true
	@docker run -d --name $(TEST_DB_NAME) --network $(TEST_NET) \
		-p $(TEST_DB_PORT):5432 \
		-e POSTGRES_USER=hookfan -e POSTGRES_PASSWORD=hookfan -e POSTGRES_DB=hookfan_test \
		postgres:16-alpine >/dev/null
	@echo "waiting for test database..."
	@for i in $$(seq 1 30); do \
		docker exec $(TEST_DB_NAME) pg_isready -U hookfan -d hookfan_test >/dev/null 2>&1 && break; \
		sleep 1; \
	done
	-@$(TEST_RUN) test ./... -count=1
	@docker rm -f $(TEST_DB_NAME) >/dev/null 2>&1 || true
	@docker network rm $(TEST_NET) >/dev/null 2>&1 || true

build:
	$(GO) build -ldflags="-s -w" -o /dev/null ./cmd/hookfan

# --- UI -------------------------------------------------------------------

ui-install:
	cd ui && npm ci

## ui-dev — Vite dev server on :5173, proxying /api to the local API
ui-dev:
	cd ui && npm run dev

ui-build:
	cd ui && npm run build

ui-check:
	cd ui && npx tsc -b

# --- Demo -----------------------------------------------------------------

## demo — bring up two receivers and show one webhook fanning out to both
demo:
	@./demo/demo.sh

demo-clean:
	@docker rm -f demo-recv-orders demo-recv-analytics >/dev/null 2>&1 || true
	@echo "demo receivers removed"

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f api

psql:
	docker compose exec db psql -U hookfan -d hookfan

# --- Migrations -----------------------------------------------------------
# Migrations also run automatically at API startup; these targets are for
# inspecting and rolling back by hand. All of them take the same advisory lock
# the API does, so running one against a live stack is safe.

migrate-up:
	docker compose run --rm --no-deps api migrate up

## migrate-down — roll back exactly one migration. Destructive.
migrate-down:
	docker compose run --rm --no-deps api migrate down

migrate-status:
	docker compose run --rm --no-deps api migrate status

## migrate-create NAME=add_foo — scaffold a new timestamped migration
migrate-create:
	@test -n "$(NAME)" || (echo "usage: make migrate-create NAME=add_something" && exit 1)
	$(GORUN) go run github.com/pressly/goose/v3/cmd/goose@latest \
		-dir internal/store/migrations create $(NAME) sql

clean:
	docker volume rm -f $(GOCACHE_VOL) $(GOMOD_VOL)
