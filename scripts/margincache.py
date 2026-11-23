"""margin 的结果缓存(内容寻址): (变异, 种子) → 验收的那一行结果("✓ 抓到 … 差分 3 / …")。

键 = 下面全部内容的 sha256。宁可多算进去(多算只是缓存不命中,少算会拿到过期的结果):
  - 变异: 名字 / 文件 / 替换式 / 原文件的内容 / 变异后的内容
  - 全局: 除了明确无关的(design/ docs/ examples/ .github/ 说明文件 …)以外,所有不按领域划分的受版本管理的文件
    —— factory/ 全部(验收 / 差分 / 参考求值器 / core / 生成器 …)、cmd/、db/、contracts/、runtime/、go.mod、go.sum、
    factory.yaml、gen/db/db.go、margin 自己的脚本 … 工厂核心一改,全部失效
  - 领域: 变异的受影响范围所在领域的全部文件(specs/<领域>/ = 式样书和人写的用例、gen/acceptance/<领域>.* = 自动生成的用例、
    internal/handler/<领域>/、proto/factory/<领域>/、gen/factory/<领域>/、gen/db/<领域>.*);范围推不出来 = 全部领域
  - Go 的版本、种子、早停的门槛
目录: MARGIN_CACHE(默认 .factory/margin-cache);CI 用 actions/cache 保存(MARGIN_CACHE_PRUNE=1: 只留这一次用到的)。
NO_CACHE=1: 不读缓存(全部重新执行,结果照样写入)。
"""
import hashlib
import json
import os
import re
import subprocess

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
DIR = os.environ.get("MARGIN_CACHE") or os.path.join(ROOT, ".factory", "margin-cache")
READ = os.environ.get("NO_CACHE") != "1"
VERSION = "margin-cache v1"

# 明确和验收的结果无关的(其余全部算进键里)
IGNORE = re.compile(r"^(design/|docs/|examples/|\.github/|scripts/(?!mutate-par\.py$|parlib\.py$|margin\.py$|margincache\.py$|mutate\.sh$)"
                    r"|gen/api-catalog\.json$|[^/]+\.md$|LICENSE$|Dockerfile|\.dockerignore$|\.gitignore$)")
# 按领域划分的文件 → 领域
DOMAIN = [re.compile(p) for p in (
    r"^specs/(\w+)/", r"^internal/handler/(\w+)/", r"^proto/factory/(\w+)/", r"^gen/factory/(\w+)/",
    r"^gen/acceptance/(\w+)\.", r"^gen/db/(\w+)\.[^/]+\.sql\.go$")]


def _sha(*parts):
    h = hashlib.sha256()
    for p in parts:
        h.update(p if isinstance(p, bytes) else p.encode())
        h.update(b"\0")
    return h.hexdigest()


class Cache:
    def __init__(self, extra=""):
        files = subprocess.run(["git", "ls-files", "-z"], cwd=ROOT, capture_output=True, check=True).stdout.decode().split("\0")
        glob, dom = [], {}
        for f in sorted(x for x in files if x):
            if IGNORE.match(f) or not os.path.isfile(os.path.join(ROOT, f)):
                continue
            d = next((m.group(1) for m in (r.match(f) for r in DOMAIN) if m), None)
            fh = _sha(f, open(os.path.join(ROOT, f), "rb").read())
            (dom.setdefault(d, []) if d else glob).append(fh)
        gover = subprocess.run(["go", "env", "GOVERSION"], capture_output=True, text=True).stdout.strip()
        self.glob = _sha(VERSION, gover, extra, *glob)
        self.dom = {d: _sha(d, *hs) for d, hs in dom.items()}
        self.all_dom = _sha(*(f"{d}={h}" for d, h in sorted(self.dom.items())))
        self.hits = self.misses = 0
        os.makedirs(DIR, exist_ok=True)

    def key(self, mutant, original, mutated, scope, seed):
        """mutant = (名字, 文件, 替换式);scope = 式样书目录(None = 全部)。"""
        m = re.match(r"specs/(\w+)", scope or "")
        d = self.dom.get(m.group(1), "") if m else self.all_dom
        return _sha(self.glob, d, scope or "*", seed, *mutant, original, mutated)

    def get(self, key):
        if not READ:
            return None
        try:
            line = json.load(open(os.path.join(DIR, key + ".json"), encoding="utf-8"))["line"]
        except (OSError, ValueError, KeyError):
            self.misses += 1
            return None
        self.hits += 1
        return line

    def put(self, key, line):
        if not (line.startswith("  ✓ ") or line.startswith("  ✗ ")):  # 只缓存执行完的结果(执行器出错 / 编译失败不缓存)
            return
        tmp = os.path.join(DIR, f".{key}.{os.getpid()}")
        json.dump({"line": line}, open(tmp, "w", encoding="utf-8"), ensure_ascii=False)
        os.replace(tmp, os.path.join(DIR, key + ".json"))

    def prune(self, used):
        """MARGIN_CACHE_PRUNE=1: 删除这一次没有用到的条目(CI 每次保存一份新的缓存,不清理会一直变大)。"""
        if os.environ.get("MARGIN_CACHE_PRUNE") != "1":
            return
        keep = {k + ".json" for k in used}
        for f in os.listdir(DIR):
            if f.endswith(".json") and f not in keep:
                os.remove(os.path.join(DIR, f))
