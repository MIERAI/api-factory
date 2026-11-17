"""验收的并行执行: 按 API 拆开(每个 API 的差分种子 = API 名 + 种子,和顺序无关),每个 worker 一套数据库。

usage: python3 scripts/accept-par.py [--server bin/server] [--workers N] [--seeds ",s1,s2"] [式样书目录 ...]
       (默认: specs 下的每个 API;多个种子同时跑)
输出: 失败的用例 + 和串行一样的总结行。
"""
import argparse
import concurrent.futures as cf
import os
import sys
import time

sys.path.insert(0, os.path.dirname(__file__))
import parlib  # noqa: E402

ap = argparse.ArgumentParser()
ap.add_argument("--server", default="bin/server")
ap.add_argument("--workers", type=int, default=parlib.CPUS)
ap.add_argument("--seeds", default="", help='逗号分隔,空 = 默认种子。例: ",s1,s2"')
ap.add_argument("roots", nargs="*")
args = ap.parse_args()
args.seeds = args.seeds.split(",")
t0 = time.time()
roots = args.roots or ["specs"]
groups = []
for root in roots:
    root = os.path.join(parlib.ROOT, root)
    if os.path.isfile(os.path.join(root, "spec.yaml")):
        groups.append(root)
        continue
    for d, _, files in sorted(os.walk(root)):
        if "spec.yaml" in files:
            groups.append(d)
if not groups:
    sys.exit(f"✗ 没有式样书: {roots}")
parlib.build_factory()
jobs = [(s, g) for s in args.seeds for g in groups]
workers = parlib.Workers("acc", min(args.workers, len(jobs)))
server = os.path.join(parlib.ROOT, args.server)
with cf.ThreadPoolExecutor(max_workers=min(args.workers, len(jobs))) as ex:
    outs = list(ex.map(lambda j: parlib.accept(workers, server, j[0], j[1]), jobs))

fail = 0
for s in args.seeds:
    tot = [0, 0, 0, 0, 0]
    broken = []
    for (seed, g), out in zip(jobs, outs):
        if seed != s:
            continue
        c = parlib.counts(out)
        if c is None:
            broken.append(f"  ✗ {os.path.relpath(g, parlib.ROOT)}: 执行器出错 — {out.strip().splitlines()[-1] if out.strip() else ''}")
            continue
        tot = [a + b for a, b in zip(tot, c)]
        for line in out.splitlines():
            if line.startswith("  ✗"):
                print(line)
    for b in broken:
        print(b)
    label = f" seed={s}" if len(args.seeds) > 1 or s else ""
    f, n, h, a, d = tot
    if f or broken:
        fail = 1
        print(f"✗ acceptance failed{label}: {f}/{n} case(s) (人写 {h} / 自动 {a} / 差分 {d})")
    else:
        print(f"✓ acceptance passed{label} ({n} case(s): 人写 {h} / 自动 {a} / 差分 {d})")
print(f"⏱ {time.time() - t0:.1f}s({len(groups)} 个 API × {len(args.seeds)} 个种子,worker {min(args.workers, len(jobs))})")
sys.exit(fail)
