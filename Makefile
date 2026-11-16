# API 工厂: 本地流水线
FACTORY_DB_PORT ?= 55432
DB_URL  ?= postgres://factory:factory@127.0.0.1:$(FACTORY_DB_PORT)/factory?sslmode=disable
FACTORY := go run ./factory/cmd/factory
SPEC    ?= specs/jobs/get-job-status/spec.yaml

.PHONY: db-up db-down db-schema check prepare build guard accept loop reset-handlers

db-up:            ## 启动本地 Postgres 并应用 schema
	@docker start api-factory-pg >/dev/null 2>&1 || docker run -d --name api-factory-pg \
		-e POSTGRES_USER=factory -e POSTGRES_PASSWORD=factory -e POSTGRES_DB=factory \
		-p $(FACTORY_DB_PORT):5432 postgres:16-alpine >/dev/null
	@until docker exec api-factory-pg psql -U factory -d factory -c "select 1" >/dev/null 2>&1; do sleep 0.5; done
	@$(MAKE) --no-print-directory db-schema

db-schema:
	@docker exec -i api-factory-pg psql -q -U factory -d factory -v ON_ERROR_STOP=1 < db/schema.sql
	@echo "✓ db ready ($(DB_URL))"

db-down:
	@docker rm -f api-factory-pg >/dev/null 2>&1 || true

check:            ## ① 式样书闸门
	@$(FACTORY) check
	@buf lint

prepare:          ## ② 确定性生成
	@$(FACTORY) prepare

build:            ## 编译服务
	@go build -o bin/server ./cmd/server
	@go vet ./...

guard:            ## ④ 路径闸门
	@$(FACTORY) guard

accept: build     ## ⑤ 验收
	@$(FACTORY) accept --server bin/server --db '$(DB_URL)'

loop:             ## 整条循环: 式样书 → 生成 → 闸门 → 验收
	@./scripts/loop.sh $(SPEC)
