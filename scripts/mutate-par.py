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
import margincache  # noqa: E402
import parlib  # noqa: E402

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
os.chdir(ROOT)
ap = argparse.ArgumentParser()
ap.add_argument("--workers", type=int, default=parlib.CPUS)
ap.add_argument("--min", type=int, default=int(os.environ.get("MIN", "3")))
ap.add_argument("seeds", nargs="*")
args = ap.parse_args()
# 早停: 一个变异的差分用例失败够 MIN 个就停止它的验收(判定和全部执行一样: 够 MIN 的照样够,不够的不会早停)。
# 报告里的差分次数因此最多是 MIN。全部执行(看真实的次数): EARLY_STOP=0
STOP = 0 if os.environ.get("EARLY_STOP") == "0" else args.min
# 种子: 参数 > MARGIN_SEED_COUNT(前 N 个: 1 = 只有默认的种子,CI 的 PR 用)> 默认 3 个
seeds = args.seeds or ["", "s1", "s2"][:int(os.environ.get("MARGIN_SEED_COUNT") or 3)]
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


# ---- 1. 每个变异: 改出临时文件 → 编译 + 生成的单元测试(和种子无关,每个变异只做一次)
#   合编(factory/mutant): 能合编的变异编进同一个服务器,运行时 FACTORY_MUTANT=<序号> 选择 → 只编译一次。
#   合编不了的(形状不支持)/ SCHEMATA=0: 和以前一样每个变异 go build -overlay 一个服务器。
#   单元测试只跑变异的文件所在的包(以前是 ./internal/...;差分的判定不看单元测试,它只影响"抓到 / 漏掉"的显示)。
def mutate(i):
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
    return {"dir": d, "file": mf, "overlay": ov}


def has_tests(pkg):
    return any(f.endswith("_test.go") for f in os.listdir(os.path.join(ROOT, pkg)))


def unit_fails(stdout):
    return len(re.findall(r"^--- FAIL", stdout, re.M))


def build_alone(i):  # 单独编译(以前的方式)
    p = prepared[i]
    name, path, _ = muts[i]
    server = os.path.join(p["dir"], "server")
    tb = time.time()
    if parlib.sh(["go", "build", "-overlay", p["overlay"], "-o", server, "./cmd/server"]).returncode:
        return {"skip": f"  ? {name}: 编译失败"}
    tt = time.time()
    pkg = os.path.dirname(path)
    unit = unit_fails(parlib.sh(["go", "test", "-overlay", p["overlay"], "./" + pkg]).stdout) if has_tests(pkg) else 0
    return {**p, "server": server, "unit": unit, "build_s": tt - tb, "test_s": time.time() - tt}


with cf.ThreadPoolExecutor(max_workers=parlib.CPUS) as ex:
    prepared = list(ex.map(mutate, range(len(muts))))
live = [i for i, p in enumerate(prepared) if "skip" not in p]
live_all = list(live)
# ---- 结果缓存(scripts/margincache.py): 命中的 (种子, 变异) 不再执行;所有种子都命中的变异连编译也省掉
cache = margincache.Cache(extra=f"stop={STOP}")
keys, cached = {}, {}
for i in live:
    orig, mutated = open(muts[i][1], "rb").read(), open(prepared[i]["file"], "rb").read()
    for sd in seeds:
        keys[(sd, i)] = cache.key(muts[i], orig, mutated, scope(muts[i][1]), sd)
        if (line := cache.get(keys[(sd, i)])) is not None:
            cached[(sd, i)] = line
live = [i for i in live if any((sd, i) not in cached for sd in seeds)]
alone = list(live)
compile_note = ""
if os.environ.get("SCHEMATA") != "0" and live:
    tb = time.time()
    sdir = os.path.join(work, "schemata")
    os.makedirs(sdir, exist_ok=True)
    spec = [{"id": str(i), "path": muts[i][1], "mutated": prepared[i]["file"]} for i in live]
    r = parlib.sh(["go", "run", "./factory/cmd/mutant-schemata", ROOT, sdir], input=json.dumps(spec))
    sch = json.loads(r.stdout) if r.returncode == 0 else None
    server = os.path.join(sdir, "server")
    if sch and sch["schemata"] and parlib.sh(["go", "build", "-overlay", sch["overlay"], "-o", server, "./cmd/server"]).returncode == 0:
        ids = {int(x) for x in sch["schemata"]}
        alone = [i for i in live if i not in ids]
        tt = time.time()
        # 单元测试: 每个包编译一次测试二进制,每个变异用 FACTORY_MUTANT 跑一次(在包的目录里,和 go test 一样)
        pkgs = sorted({os.path.dirname(muts[i][1]) for i in ids if has_tests(os.path.dirname(muts[i][1]))})

        def test_bin(pkg):
            b = os.path.join(sdir, pkg.replace("/", "_") + ".test")
            ok = parlib.sh(["go", "test", "-c", "-overlay", sch["overlay"], "-o", b, "./" + pkg]).returncode == 0
            return pkg, b if ok else None

        with cf.ThreadPoolExecutor(max_workers=parlib.CPUS) as ex:
            bins = dict(ex.map(test_bin, pkgs))

        def unit_of(i):
            b = bins.get(os.path.dirname(muts[i][1]))
            if not b:
                return 0
            env = {**os.environ, "FACTORY_MUTANT": str(i)}
            return unit_fails(parlib.sh([b], cwd=os.path.join(ROOT, os.path.dirname(muts[i][1])), env=env).stdout)

        with cf.ThreadPoolExecutor(max_workers=parlib.CPUS) as ex:
            units = dict(zip(sorted(ids), ex.map(unit_of, sorted(ids))))
        if any(b is None for b in bins.values()):  # 测试编译不了: 这些包的变异单独编译(和以前一样的结果)
            bad = {p for p, b in bins.items() if b is None}
            alone += [i for i in sorted(ids) if os.path.dirname(muts[i][1]) in bad]
            ids -= set(alone)
        for i in ids:
            prepared[i] = {**prepared[i], "server": server, "env": {"FACTORY_MUTANT": str(i)}, "unit": units[i],
                           "build_s": tt - tb, "test_s": time.time() - tt}
        compile_note = f";合编 {len(ids)} 个(编译 {tt - tb:.0f}s + 单元测试 {time.time() - tt:.0f}s),单独编译 {len(alone)} 个"
        if os.environ.get("VERBOSE"):
            for k, why in sorted(sch["fallback"].items(), key=lambda kv: int(kv[0])):
                print(f"  (单独编译 {muts[int(k)][0]}: {why})")
    else:
        compile_note = ";✗ 合编失败,全部单独编译"
        print("⚠ 变异体合编失败,全部单独编译: " + (r.stderr.strip().splitlines() or ["?"])[-1] if not sch else "⚠ 合编的服务器编译失败,全部单独编译", file=sys.stderr)
with cf.ThreadPoolExecutor(max_workers=parlib.CPUS) as ex:
    for i, p in zip(alone, ex.map(build_alone, alone)):
        prepared[i] = p
t1 = time.time()


# ---- 2. 每个 worker 一套数据库(scripts/parlib.py)
jobs = [(s, i) for s in seeds for i in range(len(muts)) if (s, i) not in cached]
workers = parlib.Workers("mut", args.workers) if jobs else None


# ---- 3. (种子 × 变异)的验收
def accept(job):
    seed, i = job
    p = prepared[i]
    if "skip" in p:
        return p["skip"]
    ta = time.time()
    out = parlib.accept(workers, p["server"], seed, scope(muts[i][1]), STOP, p.get("env"))
    name = muts[i][0]
    if STATS is not None:
        res = [l for l in out.splitlines() if l.startswith("  ✓ [") or l.startswith("  ✗ [")]
        kills = [k for k, l in enumerate(res) if l.startswith("  ✗") and " 差分]" in l]
        STATS.append({"seed": seed, "i": i, "name": name, "scope": scope(muts[i][1]), "cases": len(res),
                      "kill_idx": kills[:args.min], "kills": len(kills), "accept_s": time.time() - ta,
                      "build_s": p.get("build_s"), "test_s": p.get("test_s")})
    if "case(s)" not in out:
        if os.environ.get("VERBOSE"):  # 执行器出错: 输出的最后几行(stderr)
            print(f"--- {name} (seed={seed or '默认'}):\n" + "\n".join(out.strip().splitlines()[-6:]), file=sys.stderr)
        return f"  ? {name}: 执行器出错 — {out.strip().splitlines()[-1] if out.strip() else ''}"
    fails = [l for l in out.splitlines() if l.startswith("  ✗")]
    diff = sum(1 for l in fails if " 差分]" in l)
    auto = sum(1 for l in fails if " auto:" in l)
    human = len(fails) - diff - auto
    caught = diff > 0 or auto > 0 or p["unit"] > 0
    return "  %s %-36s 差分 %2d / 自动用例 %2d / 生成的单元测试 %d / 人写 %2d" % (
        "✓ 抓到" if caught else "✗ 漏掉", name, diff, auto, p["unit"], human)


STATS = [] if os.environ.get("MARGIN_STATS") else None
with cf.ThreadPoolExecutor(max_workers=args.workers) as ex:
    done = dict(zip(jobs, ex.map(accept, jobs)))
for job, line in done.items():
    if job in keys:
        cache.put(keys[job], line)
cache.prune(keys.values())  # MARGIN_CACHE_PRUNE=1(CI): 这一次没用到的条目删掉(缓存不会无限增长)
lines = [cached.get((s, i)) or done[(s, i)] for s in seeds for i in range(len(muts))]
t2 = time.time()

fail = 0
for k, s in enumerate(seeds):
    text = "\n".join(lines[k * len(muts):(k + 1) * len(muts)]) + "\n"
    if os.environ.get("VERBOSE"):
        print(text, end="")
    r = subprocess.run([sys.executable, "scripts/margin.py", s, str(args.min), str(len(muts))], input=text, text=True)
    fail |= r.returncode
if STATS is not None:
    json.dump(STATS, open(os.environ["MARGIN_STATS"], "w"), ensure_ascii=False, indent=0)
scoped = sum(1 for m in muts if scope(m[1]))
hit = f";缓存命中 {len(cached)}/{len(seeds) * len(live_all)}" + ("" if margincache.READ else "(NO_CACHE=1: 不读缓存)")
stop = f";早停 {STOP}" if STOP else ";不早停"
print(f"⏱ 编译 {t1 - t0:.0f}s{compile_note} + 验收 {t2 - t1:.0f}s(变异 {len(muts)} × 种子 {len(seeds)},worker {args.workers};只验收受影响的 API: {scoped}/{len(muts)}{hit}{stop})")
sys.exit(fail)
