"""margin.sh 的解析: mutate.sh 的输出 → 每个变异被几个差分用例抓到。"""
import re
import sys

seed, mn = sys.argv[1], int(sys.argv[2])
rows = []
for line in sys.stdin:
    m = re.search(r"(抓到|漏掉) (.+?)\s+差分\s+(\d+)", line)
    if m:
        rows.append((int(m.group(3)), m.group(1), m.group(2).strip()))
counted = [r for r in rows if "[单元测试负责]" not in r[2]]
weak = [r for r in counted if r[0] < mn or r[1] == "漏掉"]
lo = min(counted, default=(0, "", "-"))
label = seed or "(默认)"
msg = f"seed={label}: 变异 {len(rows)} 个,差分最少 {lo[0]}({lo[2]})"
if weak:
    msg += f"  ✗ 不足 {mn}: {[(r[0], r[2]) for r in weak]}"
print(msg)
sys.exit(1 if weak or not rows else 0)
