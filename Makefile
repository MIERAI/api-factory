# API 工厂: 本地流水线
FACTORY_DB_PORT ?= 55432
PG_NAME ?= api-factory-pg
DB_URL  ?= postgres://factory:factory@127.0.0.1:$(FACTORY_DB_PORT)/factory?sslmode=disable
# 分库(按店铺分库的表): 测试用两个库,店铺 1~2 → shard_a,其他 → shard_b
SHARD_URL = postgres://factory:factory@127.0.0.1:$(FACTORY_DB_PORT)
export FACTORY_SHARDS ?= 1-2=$(SHARD_URL)/shard_a?sslmode=disable;*=$(SHARD_URL)/shard_b?sslmode=disable
FACTORY := go run ./factory/cmd/factory
# 镜像的来源: Docker Hub 的官方镜像经 Google 的缓存 mirror.gcr.io 拉(不受 Docker Hub 匿名拉取的次数限制,
# 不需要登录)。换来源只改这一处(scripts/ci、e2e 的 compose、Dockerfile 的 ARG 都读同名变量)。boundaries.md §8
export IMAGE_MIRROR ?= mirror.gcr.io/library
PG_IMAGE ?= $(IMAGE_MIRROR)/postgres:16-alpine
MQ_IMAGE ?= $(IMAGE_MIRROR)/rabbitmq:3.13.7-management
SPEC    ?= specs/jobs/get-job-status/spec.yaml
MODEL   ?= claude
ACCEPT_SPECS ?= specs

.PHONY: e2e projects perf hooks margin doctor mq-up mq-down accept-mq db-up db-down db-schema design check prepare build test guard accept loop review-local review-fetch

# 排序规则用 ICU(en-US): Alpine 的 en_US.utf8 实际按字节排,和 C 一样,测不出"忘了 COLLATE C"(生产不是这样)
hooks:            ## 安装 git 钩子(内容防护: 产品分支不能出现试用公司的内容)
	@git config core.hooksPath scripts/hooks && echo "✓ hooks: scripts/hooks"

perf:             ## 性能闸门: 本仓库 + examples/ 的项目,每表 20 万行 + EXPLAIN ANALYZE(独立的新 Postgres 容器;慢,不在 accept 里)
	@PERF_ARGS='$(PERF_ARGS)' scripts/perf.sh $(PERF_DIRS)

margin:           ## 差分测试的余量: 换种子跑变异,每个变异 ≥3 个差分用例(并行: go build -overlay + 每个 worker 一套数据库)
	@DB_URL='$(DB_URL)' PG_NAME=$(PG_NAME) scripts/margin.sh

MQ_NAME ?= api-factory-mq
MQ_PORT ?= 55672
mq-up:            ## RabbitMQ(消息的真实投递场景;数据目录 tmpfs,不写持久卷)
	@docker inspect $(MQ_NAME) >/dev/null 2>&1 || docker run -d --name $(MQ_NAME) --tmpfs /var/lib/rabbitmq -p $(MQ_PORT):5672 $(MQ_IMAGE) >/dev/null
	@# 探活用 rabbitmq 用户: root 的 rabbitmq-diagnostics 会抢在服务器之前建 .erlang.cookie(属主 root)→ 服务器 eacces 退出
	@until docker exec -u rabbitmq $(MQ_NAME) rabbitmq-diagnostics -q ping >/dev/null 2>&1; do \
		[ "$$(docker inspect -f '{{.State.Running}}' $(MQ_NAME))" = true ] || { docker logs --tail 20 $(MQ_NAME); exit 1; }; sleep 1; done
	@echo "✓ RabbitMQ amqp://guest:guest@127.0.0.1:$(MQ_PORT)/"
mq-down:          ## 删除 RabbitMQ 的容器
	@docker rm -f -v $(MQ_NAME) >/dev/null 2>&1 || true
accept-mq: build  ## 真实 RabbitMQ 的场景(有消息的 API;独立的数据库 + vhost,串行)
	@DB_URL='$(DB_URL)' PG_NAME=$(PG_NAME) MQ_NAME=$(MQ_NAME) MQ_PORT=$(MQ_PORT) scripts/accept-mq.sh $(ACCEPT_SPECS)

doctor:           ## 环境检查(工具 / 数据库 / AI 命令行 / 钩子)
	@scripts/doctor.sh

db-up:            ## 启动本地 Postgres 并应用 schema
	@docker start $(PG_NAME) >/dev/null 2>&1 || docker run -d --name $(PG_NAME) \
		-e POSTGRES_USER=factory -e POSTGRES_PASSWORD=factory -e POSTGRES_DB=factory \
		-e POSTGRES_INITDB_ARGS="--locale-provider=icu --icu-locale=en-US" \
		-p $(FACTORY_DB_PORT):5432 $(PG_IMAGE) -c max_connections=1000 >/dev/null
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

check: design     ## ① 式样书闸门(+ 内容防护)
	@scripts/content-guard.sh --tree
	@$(FACTORY) check
	@$(FACTORY) doc --check
	@buf lint

prepare:          ## ② 确定性生成
	@$(FACTORY) prepare

build:            ## 编译服务
	@go build -o bin/server ./cmd/server
	@go vet ./...

guard:            ## ④ 路径闸门
	@$(FACTORY) guard

test:             ## 单元测试(含工厂生成的 enum / 逻辑样例测试)
	@go test ./internal/...

accept: build test  ## 验收(ACCEPT_SPECS 可限定为某个式样书目录;SEEDS=",s1,s2" 多个种子同时跑;SERIAL=1 串行)
ifdef SERIAL
	@go build -o .factory/factory ./factory/cmd/factory
	@.factory/factory serve-test --server bin/server --db '$(DB_URL)' -- .factory/factory accept --db '$(DB_URL)' $(ACCEPT_SPECS)
else
	@DB_URL='$(DB_URL)' PG_NAME=$(PG_NAME) python3 scripts/accept-par.py $(if $(SEEDS),--seeds '$(SEEDS)') $(ACCEPT_SPECS)
endif

projects:         ## 其他项目(examples/ 下有 factory.yaml 的目录,factory --project): 设计 → 生成物最新 → 编译 → 验收
	@scripts/projects.sh

e2e:              ## storefront v2 端到端(手写层,examples/storefront-v2/e2e): compose 起 RabbitMQ + 拓扑 + 5 个服务 → 驱动 → down -v
	@$(MAKE) --no-print-directory -C examples/storefront-v2/e2e e2e

review-local:     ## 推送前的 AI 审查(和 CI 的 spec-pr 同一个脚本: 本仓库 + examples/ 的项目;本地 claude CLI;BASE=origin/main)
	@rm -f .factory/review-local.md
	@CI_SUMMARY=.factory/review-local.md BASE=$(or $(BASE),origin/main) REVIEW_MODEL=$(or $(REVIEW_MODEL),claude) scripts/ci/spec-review.sh; \
		rc=$$?; echo "结果: .factory/review-local.md"; exit $$rc

review-fetch:     ## CI 的审查结果下载到 [<项目>/].factory/reviews + 轮次记录(PR=25 / BRANCH=x;默认当前分支)→ factory answer
	@scripts/review-fetch.sh $(if $(PR),--pr $(PR)) $(if $(BRANCH),--branch $(BRANCH))

loop:             ## 整条流水线: 闸门 → 审查 → 生成 → AI → 闸门 → 验收 (MODEL=claude|claude-sonnet)
	@./scripts/loop.sh $(SPEC) $(MODEL)
