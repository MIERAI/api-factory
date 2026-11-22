"""变异测试的并行执行(margin 用)。

scripts/mutate.sh 是变异的清单(PLAN=1 时只列出)。这里:
  1. 每个变异: 用 perl 改出一份临时文件,`go build -overlay` 编译出自己的服务器(工作区的文件不动),
     `go test -overlay` 跑生成的单元测试 —— 和种子无关,每个变异只做一次,并行
  2. (种子 × 变异)的验收: W 个 worker 并行,每个 worker 一套自己的数据库(主库 + 两个分库)
  3. 输出和 mutate.sh 一样的行,每个种子交给 margin.py 判定

usage: python3 scripts/mutate-par.py [--workers N] [--min 3] [seed ...]    (默认种子: "" s1 s2)
环境: DB_URL(主库,决定端口)/ PG_NAME(容器)和 Makefile 一样
"""
import argparse
import concurrent.futures as cf
import json
import os
import re
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(__file__))
import parlib  # noqa: E402

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
os.chdir(ROOT)
ap = argparse.ArgumentParser()
ap.add_argument("--workers", type=int, default=parlib.CPUS)
ap.add_argument("--min", type=int, default=int(os.environ.get("MIN", "3")))
ap.add_argument("seeds", nargs="*")
args = ap.parse_args()
seeds = args.seeds or ["", "s1", "s2"]
work = os.path.join(ROOT, ".factory", f"mut-{os.getpid()}")  # 这一次运行自己的(同时跑两个 margin 也不冲突)
os.makedirs(work, exist_ok=True)
import atexit, shutil  # noqa: E401,E402
atexit.register(lambda: shutil.rmtree(work, ignore_errors=True))
t0 = time.time()


# ---- 变异的清单
raw = subprocess.run(["bash", "scripts/mutate.sh"], env={**os.environ, "PLAN": "1"}, capture_output=True).stdout.decode()
parts = raw.split("\0")[:-1]
muts = [tuple(parts[i:i + 3]) for i in range(0, len(parts), 3)]
if os.environ.get("MARGIN_SHARD"):  # CI 的分片 "k/N": 清单里第 i 个变异归 i % N == k 的分片(N 个分片合起来 = 全部,判定不变)
    k, n = (int(x) for x in os.environ["MARGIN_SHARD"].split("/"))
    assert 0 <= k < n, "MARGIN_SHARD 写成 k/N(0 <= k < N)"
    muts = [m for i, m in enumerate(muts) if i % n == k]
if os.environ.get("MUTANTS"):  # 只跑名字匹配的变异(迭代时用;margin 的门槛要全部)
    muts = [m for m in muts if re.search(os.environ["MUTANTS"], m[0])]

# ---- 工厂本身(不变异)
parlib.build_factory()


# 变异改的文件 → 受影响的式样书目录(只验收这一部分;推不出来时验收全部)。
# 变异只影响用到这个文件的 API,判定(抓到几次)和验收全部时一样,时间不随 API 的总数增长。
def scope(path):
    m = re.match(r"gen/db/(\w+)\.([\w-]+)\.sql\.go$", path)
    if m:
        d = f"specs/{m.group(1)}/{m.group(2)}"
    elif (m := re.match(r"internal/handler/(\w+)/(?:enums|commands)_gen\.go$", path)):  # 每个服务一个: 整个领域
        d = f"specs/{m.group(1)}"
    elif (m := re.match(r"internal/handler/(\w+)/(\w+)_gen\.go$", path)):
        d = f"specs/{m.group(1)}/{m.group(2).replace('_', '-')}"
    else:
        return None
    return d if os.path.isdir(os.path.join(ROOT, d)) else None


# ---- 1. 每个变异: 改出临时文件 → overlay 编译 + 单元测试
def prepare(i):
    name, path, expr = muts[i]
    d = os.path.join(work, str(i))
    os.makedirs(d, exist_ok=True)
    src = open(path, encoding="utf-8").read()
    out = subprocess.run(["perl", "-0pe", expr], input=src.encode(), capture_output=True).stdout.decode()
    if out == src:
        return {"skip": f"  ? {name}: 替换式没有命中"}
    mf = os.path.join(d, os.path.basename(path))
    open(mf, "w", encoding="utf-8").write(out)
    ov = os.path.join(d, "overlay.json")
    json.dump({"Replace": {os.path.join(ROOT, path): mf}}, open(ov, "w"))
    server = os.path.join(d, "server")
    if parlib.sh(["go", "build", "-overlay", ov, "-o", server, "./cmd/server"]).returncode:
        return {"skip": f"  ? {name}: 编译失败"}
    t = parlib.sh(["go", "test", "-overlay", ov, "./internal/..."])
    unit = len(re.findall(r"^--- FAIL", t.stdout, re.M))
    return {"server": server, "unit": unit}


with cf.ThreadPoolExecutor(max_workers=os.cpu_count() or 4) as ex:
    prepared = list(ex.map(prepare, range(len(muts))))
t1 = time.time()


# ---- 2. 每个 worker 一套数据库(scripts/parlib.py)
workers = parlib.Workers("mut", args.workers)


# ---- 3. (种子 × 变异)的验收
def accept(job):
    seed, i = job
    p = prepared[i]
    if "skip" in p:
        return p["skip"]
    out = parlib.accept(workers, p["server"], seed, scope(muts[i][1]))
    name = muts[i][0]
    if "case(s)" not in out:
        return f"  ? {name}: 执行器出错 — {out.strip().splitlines()[-1] if out.strip() else ''}"
    fails = [l for l in out.splitlines() if l.startswith("  ✗")]
    diff = sum(1 for l in fails if " 差分]" in l)
    auto = sum(1 for l in fails if " auto:" in l)
    human = len(fails) - diff - auto
    caught = diff > 0 or auto > 0 or p["unit"] > 0
    return "  %s %-36s 差分 %2d / 自动用例 %2d / 生成的单元测试 %d / 人写 %2d" % (
        "✓ 抓到" if caught else "✗ 漏掉", name, diff, auto, p["unit"], human)


jobs = [(s, i) for s in seeds for i in range(len(muts))]
with cf.ThreadPoolExecutor(max_workers=args.workers) as ex:
    lines = list(ex.map(accept, jobs))
t2 = time.time()

fail = 0
for k, s in enumerate(seeds):
    text = "\n".join(lines[k * len(muts):(k + 1) * len(muts)]) + "\n"
    if os.environ.get("VERBOSE"):
        print(text, end="")
    r = subprocess.run([sys.executable, "scripts/margin.py", s, str(args.min), str(len(muts))], input=text, text=True)
    fail |= r.returncode
scoped = sum(1 for m in muts if scope(m[1]))
print(f"⏱ 编译 {t1 - t0:.0f}s + 验收 {t2 - t1:.0f}s(变异 {len(muts)} × 种子 {len(seeds)},worker {args.workers};只验收受影响的 API: {scoped}/{len(muts)})")
sys.exit(fail)
