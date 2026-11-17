#!/usr/bin/env bash
# 阶段 4: 生成物合并到 main 之后。镜像 → 部署 → 部署后的检查。失败时旧版本继续服务。
#   SIM_BREAK=1(只对 local): 新版本连不上库 → 检查失败 → 回滚(确认旧版本继续服务)
#   TARGET=local    本地模拟 Cloud Run: docker 里起新版本 → 健康检查(版本号)→ 认证 → 对它跑验收 → 切换,停掉旧版本
#                   (验收会清空 / 灌入测试数据: 只能对模拟用的库)
#   TARGET=cloudrun Artifact Registry → gcloud run deploy(--no-traffic)→ 新版本的只读检查 → 切流量。DRY_RUN=1: 只打印命令
source "$(dirname "$0")/lib.sh"
TARGET=${TARGET:-local}
VERSION=${VERSION:-$(git rev-parse --short HEAD)}
md "## 部署($TARGET,$VERSION)" "" "| 步骤 | 时间 |" "|---|---|"

case $TARGET in
local)
  NET=api-factory-sim; DB=api-factory-sim-db; IMAGE=api-factory:$VERSION
  TOKEN=${API_TOKEN:-$(cat .factory/sim-token 2>/dev/null || echo sim-token-$(date +%s))}
  echo "$TOKEN" > .factory/sim-token   # 本地模拟: 手动调用用(curl -H "Authorization: Bearer $(cat .factory/sim-token)")
  HASH=$(printf %s "$TOKEN" | shasum -a 256 | cut -d' ' -f1)
  DBURL=postgres://factory:factory@127.0.0.1:${SIM_DB_PORT:-55440}/factory?sslmode=disable
  simdb() { # 模拟用的库(和本地开发的库分开): 结构来自 db/schema
    docker network inspect $NET >/dev/null 2>&1 || docker network create $NET >/dev/null
    docker start $DB >/dev/null 2>&1 || docker run -d --name $DB --network $NET -e POSTGRES_USER=factory -e POSTGRES_PASSWORD=factory -e POSTGRES_DB=factory \
      -e POSTGRES_INITDB_ARGS="--locale-provider=icu --icu-locale=en-US" -p ${SIM_DB_PORT:-55440}:5432 postgres:16-alpine >/dev/null
    until docker exec $DB psql -U factory -d factory -c 'select 1' >/dev/null 2>&1; do sleep 0.5; done
    for s in shard_a shard_b; do docker exec $DB psql -q -U factory -tAc "SELECT 1 FROM pg_database WHERE datname='$s'" | grep -q 1 || docker exec $DB psql -q -U factory -c "CREATE DATABASE $s" >/dev/null; done
    for d in factory shard_a shard_b; do cat db/schema/*.sql | docker exec -i $DB psql -q -U factory -d $d -v ON_ERROR_STOP=1 >/dev/null; done
  }
  build() { docker build -q -t "$IMAGE" --build-arg VERSION="$VERSION" . >/dev/null; }
  run_new() { # 新版本(旧版本还在服务)
    docker rm -f api-factory-sim-$VERSION >/dev/null 2>&1 || true
    docker run -d --name api-factory-sim-$VERSION --network $NET -p 0:8080 \
      -e DATABASE_URL="postgres://factory:factory@${SIM_BREAK:+broken-}$DB:5432/factory?sslmode=disable" \
      -e FACTORY_SHARDS="1-2=postgres://factory:factory@$DB:5432/shard_a?sslmode=disable;*=postgres://factory:factory@$DB:5432/shard_b?sslmode=disable" \
      -e AUTH_MODE=token -e AUTH_TOKEN_SHA256="$HASH" -e PAGE_TOKEN_KEY=sim-page-key "$IMAGE" >/dev/null
    PORT=$(docker port api-factory-sim-$VERSION 8080/tcp | head -1 | awk -F: '{print $NF}')
    URL=http://127.0.0.1:$PORT
  }
  healthy() { # 新版本自己回答(响应头里的版本号)
    for _ in $(seq 1 60); do
      v=$(curl -s -D - -o /dev/null "$URL/healthz" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-app-version"{print $2}')
      [[ $v == "$VERSION" ]] && return 0
      sleep 0.5
    done
    docker logs api-factory-sim-$VERSION | tail -20; return 1
  }
  authn() { # 没有令牌 → 401;有令牌 → 不是 401
    local svc; svc=$(grep -m1 'rpc:' specs/*/*/spec.yaml | head -1 | awk '{print $NF}')
    local path="/${svc%/*}/${svc##*/}"
    [[ $(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{}' "$URL$path") == 401 ]] || { echo "✗ 没有令牌也能调用"; return 1; }
    [[ $(curl -s -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -H "Authorization: Bearer $TOKEN" -d '{}' "$URL$path") != 401 ]] || { echo "✗ 令牌被拒绝"; return 1; }
  }
  smoke() { # 对新版本跑验收(人写 + 自动 + 差分;用模拟的库)
    API_TOKEN=$TOKEN go run ./factory/cmd/factory accept --url "$URL" --db "$DBURL" ${SMOKE_SPECS:-specs} 2>&1 | tail -3
    return "${PIPESTATUS[0]}"
  }
  switch() { # 切换: 记下当前的版本,停掉旧版本
    old=$(cat .factory/sim-current 2>/dev/null || true)
    echo "$VERSION $URL" > .factory/sim-current
    if [[ -n $old && ${old%% *} != "$VERSION" ]]; then docker rm -f "api-factory-sim-${old%% *}" >/dev/null 2>&1 || true; fi
  }
  rollback() { echo "✗ 新版本没有通过检查: 停掉新版本,旧版本继续服务"; docker rm -f api-factory-sim-$VERSION >/dev/null 2>&1 || true; md "| ✗ 回滚(旧版本继续服务) | |"; exit 1; }
  stage "模拟用的库" simdb
  stage "镜像" build
  stage "新版本启动" run_new
  stage "健康检查(新版本的版本号)" healthy || rollback
  stage "认证" authn || rollback
  FACTORY_SHARDS="1-2=postgres://factory:factory@127.0.0.1:${SIM_DB_PORT:-55440}/shard_a?sslmode=disable;*=postgres://factory:factory@127.0.0.1:${SIM_DB_PORT:-55440}/shard_b?sslmode=disable" \
    stage "部署后的验收(对新版本)" smoke || rollback
  stage "切换" switch
  md "" "新版本: $URL"
  ;;
cloudrun)
  : "${GCP_PROJECT:?}" "${GCP_REGION:?}" "${SERVICE:?}" "${AR_REPO:?}" "${RUN_SA:?服务账号}"
  IMAGE=$GCP_REGION-docker.pkg.dev/$GCP_PROJECT/$AR_REPO/api:$VERSION
  run() { if [[ -n ${DRY_RUN:-} ]]; then echo "  [dry-run] $*"; else "$@"; fi; }
  stage "镜像(Cloud Build)" run gcloud builds submit --project "$GCP_PROJECT" --tag "$IMAGE" --quiet
  # 认证交给 Cloud Run 的 IAM(--no-allow-unauthenticated)→ AUTH_MODE=none;连接信息在 Secret Manager
  stage "新版本(不切流量)" run gcloud run deploy "$SERVICE" --project "$GCP_PROJECT" --region "$GCP_REGION" --image "$IMAGE" \
    --service-account "$RUN_SA" --no-allow-unauthenticated --no-traffic --tag "v-$VERSION" \
    --set-env-vars "AUTH_MODE=none,APP_VERSION=$VERSION" \
    --set-secrets "DATABASE_URL=${SERVICE}-database-url:latest,PAGE_TOKEN_KEY=${SERVICE}-page-token-key:latest,FACTORY_SHARDS=${SERVICE}-shards:latest" --quiet
  check() { # 只读的检查: 新版本的 URL(tag)上的健康检查和版本号;不带身份 → 403
    local url; url=$(gcloud run services describe "$SERVICE" --project "$GCP_PROJECT" --region "$GCP_REGION" --format "value(status.traffic[?tag='v-$VERSION'].url)" 2>/dev/null || true)
    [[ -n ${DRY_RUN:-} ]] && { echo "  [dry-run] curl \$TAG_URL/healthz(ID token)"; return 0; }
    local tok; tok=$(gcloud auth print-identity-token)
    [[ $(curl -s -D - -o /dev/null -H "Authorization: Bearer $tok" "$url/healthz" | tr -d '\r' | awk -F': ' 'tolower($1)=="x-app-version"{print $2}') == "$VERSION" ]] || return 1
    [[ $(curl -s -o /dev/null -w '%{http_code}' "$url/healthz") == 403 ]]
  }
  stage "新版本的检查(只读)" check
  stage "切流量" run gcloud run services update-traffic "$SERVICE" --project "$GCP_PROJECT" --region "$GCP_REGION" --to-tags "v-$VERSION=100" --quiet
  ;;
*) echo "TARGET=local|cloudrun"; exit 2 ;;
esac
