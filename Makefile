# API 工厂: 本地流水线
FACTORY_DB_PORT ?= 55432
PG_NAME ?= api-factory-pg
DB_URL  ?= postgres://factory:factory@127.0.0.1:$(FACTORY_DB_PORT)/factory?sslmode=disable
# 分库(按店铺分库的表): 测试用两个库,店铺 1~2 → shard_a,其他 → shard_b
SHARD_URL = postgres://factory:factory@127.0.0.1:$(FACTORY_DB_PORT)
export FACTORY_SHARDS ?= 1-2=$(SHARD_URL)/shard_a?sslmode=disable;*=$(SHARD_URL)/shard_b?sslmode=disable
FACTORY := go run ./factory/cmd/factory
SPEC    ?= specs/jobs/get-job-status/spec.yaml
MODEL   ?= claude
ACCEPT_SPECS ?= specs

.PHONY: db-up db-down db-schema design check prepare build test guard accept loop

# 排序规则用 ICU(en-US): Alpine 的 en_US.utf8 实际按字节排,和 C 一样,测不出"忘了 COLLATE C"(生产不是这样)
db-up:            ## 启动本地 Postgres 并应用 schema
	@docker start $(PG_NAME) >/dev/null 2>&1 || docker run -d --name $(PG_NAME) \
		-e POSTGRES_USER=factory -e POSTGRES_PASSWORD=factory -e POSTGRES_DB=factory \
		-e POSTGRES_INITDB_ARGS="--locale-provider=icu --icu-locale=en-US" \
		-p $(FACTORY_DB_PORT):5432 postgres:16-alpine >/dev/null
	@until docker exec $(PG_NAME) psql -U factory -d factory -c "select 1" >/dev/null 2>&1; do sleep 0.5; done
	@$(MAKE) --no-print-directory db-schema

db-schema:
	@cat db/schema/*.sql $(wildcard db/views/*.sql) | docker exec -i $(PG_NAME) psql -q -U factory -d factory -v ON_ERROR_STOP=1
	@for s in shard_a shard_b; do \
		docker exec $(PG_NAME) psql -q -U factory -d factory -tAc "SELECT 1 FROM pg_database WHERE datname='$$s'" | grep -q 1 || \
			docker exec $(PG_NAME) psql -q -U factory -d factory -c "CREATE DATABASE $$s" >/dev/null; \
		cat db/schema/*.sql | docker exec -i $(PG_NAME) psql -q -U factory -d $$s -v ON_ERROR_STOP=1; \
	done
	@echo "✓ db ready ($(DB_URL))"

db-down:
	@docker rm -f $(PG_NAME) >/dev/null 2>&1 || true

design:           ## 中文式样书(design/<领域>/)→ proto + specs(生成)
	@$(FACTORY) design

check: design     ## ① 式样书闸门
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
	@go build -o .factory/factory ./factory/cmd/factory
	@.factory/factory serve-test --server bin/server --db '$(DB_URL)' -- .factory/factory accept --db '$(DB_URL)' $(ACCEPT_SPECS)

loop:             ## 整条流水线: 闸门 → 审查 → 生成 → AI → 闸门 → 验收 (MODEL=claude|local)
	@./scripts/loop.sh $(SPEC) $(MODEL)
