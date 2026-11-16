# API 工厂: 本地流水线
FACTORY_DB_PORT ?= 55432
PG_NAME ?= api-factory-pg
DB_URL  ?= postgres://factory:factory@127.0.0.1:$(FACTORY_DB_PORT)/factory?sslmode=disable
FACTORY := go run ./factory/cmd/factory
SPEC    ?= specs/jobs/get-job-status/spec.yaml
MODEL   ?= claude
ACCEPT_SPECS ?= specs

.PHONY: db-up db-down db-schema check prepare build test guard accept loop

db-up:            ## 启动本地 Postgres 并应用 schema
	@docker start $(PG_NAME) >/dev/null 2>&1 || docker run -d --name $(PG_NAME) \
		-e POSTGRES_USER=factory -e POSTGRES_PASSWORD=factory -e POSTGRES_DB=factory \
		-p $(FACTORY_DB_PORT):5432 postgres:16-alpine >/dev/null
	@until docker exec $(PG_NAME) psql -U factory -d factory -c "select 1" >/dev/null 2>&1; do sleep 0.5; done
	@$(MAKE) --no-print-directory db-schema

db-schema:
	@cat db/schema/*.sql | docker exec -i $(PG_NAME) psql -q -U factory -d factory -v ON_ERROR_STOP=1
	@echo "✓ db ready ($(DB_URL))"

db-down:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true

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

test:             ## 单元测试(含工厂生成的 enum / 逻辑样例测试)
	@go test ./internal/... ./platform/...

accept: build test  ## 验收(ACCEPT_SPECS 可限定为某个式样书目录)
	@$(FACTORY) accept --server bin/server --db '$(DB_URL)' $(ACCEPT_SPECS)

loop:             ## 整条流水线: 闸门 → 审查 → 生成 → AI → 闸门 → 验收 (MODEL=claude|local)
	@./scripts/loop.sh $(SPEC) $(MODEL)
