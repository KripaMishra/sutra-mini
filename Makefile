# Sutra Mini — run everything from the repo root. `make` lists the targets.

BACKEND := backend
WEB     := web

.PHONY: help migrate api mock worker web dev test build

help: ## list the available targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'

migrate: ## apply migrations + seed and print the row counts (idempotent)
	cd $(BACKEND) && go run ./cmd/migrate -seed -check

api: ## run the HTTP API on :8080
	cd $(BACKEND) && go run ./cmd/api

mock: ## run the mock accounting API on :8090 (20% 500, 10% hang, 409 duplicate)
	cd $(BACKEND) && go run ./cmd/mockaccounting

worker: ## run the sync worker that pushes pending vouchers
	cd $(BACKEND) && go run ./cmd/worker

web/node_modules: $(WEB)/package.json $(WEB)/package-lock.json
	cd $(WEB) && npm install
	touch $(WEB)/node_modules

web: web/node_modules ## run the /sync page on :3000
	cd $(WEB) && npm run dev

dev: migrate ## run api + mock + worker + web together; Ctrl-C stops them all
	@$(MAKE) -j4 api mock worker web

test: ## run the Go test suite against the Postgres in .env
	cd $(BACKEND) && go test ./...

build: ## build backend binaries into bin/ and the web production bundle
	mkdir -p bin
	cd $(BACKEND) && go build -o ../bin/ ./cmd/...
	cd $(WEB) && npm run build
