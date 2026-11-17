"""并行测试的公共部分: 每个 worker 一套数据库(主库 + 两个分库),验收在 worker 上跑。

margin(mutate-par.py)/ make accept(accept-par.py)/ 矩阵(matrix.sh)共用。
数据库名带上工作区路径的哈希: main、试用、矩阵的临时工作区同时跑也不会互相干扰。
环境: DB_URL(主库,决定端口)/ PG_NAME(容器),和 Makefile 一样。
"""
import hashlib
import os
import queue
import re
import subprocess
from contextlib import contextmanager

ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
DB_URL = os.environ.get("DB_URL", "postgres://factory:factory@127.0.0.1:55432/factory?sslmode=disable")
BASE = DB_URL.split("/factory?")[0]
PG = os.environ.get("PG_NAME", "api-factory-pg")
CPUS = os.cpu_count() or 4


def sh(cmd, **kw):  # 输出里可能有被截断的多字节字符(报错详情按字节截断)
    return subprocess.run(cmd, capture_output=True, text=True, encoding="utf-8", errors="replace", **kw)


def _psql(db, data):
    return subprocess.run(["docker", "exec", "-i", "-e", "PGOPTIONS=-c client_min_messages=warning", PG,
                           "psql", "-q", "-U", "factory", "-d", db, "-v", "ON_ERROR_STOP=1"],
                          input=data, capture_output=True, text=True)


def _sql_files(d):
    d = os.path.join(ROOT, d)
    return sorted(os.path.join(d, f) for f in os.listdir(d) if f.endswith(".sql")) if os.path.isdir(d) else []


class Workers:
    """n 套数据库。with workers.take() as (url, shards): …"""

    def __init__(self, kind, n):
        tag = hashlib.sha1(ROOT.encode()).hexdigest()[:6]
        schema = "".join(open(f, encoding="utf-8").read() + "\n" for f in _sql_files("db/schema"))
        views = "".join(open(f, encoding="utf-8").read() + "\n" for f in _sql_files("db/views"))
        chk = sh(["docker", "exec", PG, "psql", "-U", "factory", "-d", "factory", "-tAc", "SELECT datname FROM pg_database"])
        have = set(chk.stdout.split())
        self.q = queue.Queue()
        jobs = []
        for w in range(n):
            names = [f"{kind}_{tag}_{w}", f"{kind}_{tag}_{w}_a", f"{kind}_{tag}_{w}_b"]
            for db, sql in zip(names, (schema + views, schema, schema)):
                if db not in have:
                    r = _psql("factory", f"CREATE DATABASE {db};")
                    if r.returncode and "already exists" not in r.stderr:
                        raise SystemExit(f"✗ CREATE DATABASE {db}: {r.stderr}")
                jobs.append((db, sql))
            # 测试服务器的连接池限制为 4(默认 = CPU 数;很多服务器同时跑时 Postgres 的连接会用光 → UNAVAILABLE)
            q = "sslmode=disable&pool_max_conns=4"
            self.q.put((f"{BASE}/{names[0]}?{q}", f"1-2={BASE}/{names[1]}?{q};*={BASE}/{names[2]}?{q}"))
        import concurrent.futures as cf
        with cf.ThreadPoolExecutor(max_workers=min(len(jobs), CPUS)) as ex:  # schema 也并行
            for (db, _), r in zip(jobs, ex.map(lambda j: _psql(j[0], j[1]), jobs)):
                if r.returncode:
                    raise SystemExit(f"✗ {db} 的 schema: {r.stderr}")

    @contextmanager
    def take(self):
        w = self.q.get()
        try:
            yield w
        finally:
            self.q.put(w)


def accept(workers, server, seed="", specs=None):
    """在一个 worker 上跑验收,返回输出。specs: 式样书目录(None = 全部)。"""
    with workers.take() as (url, shards):
        env = {**os.environ, "FACTORY_DIFF_SEED": seed, "FACTORY_SHARDS": shards}
        cmd = [FACTORY, "serve-test", "--server", server, "--db", url, "--", FACTORY, "accept", "--db", url]
        if specs:
            cmd.append(specs)
        r = sh(cmd, env=env, cwd=ROOT)
    return r.stdout + r.stderr


CASES = re.compile(r"(\d+)/(\d+) case\(s\) \(人写 (\d+) / 自动 (\d+) / 差分 (\d+)\)|\((\d+) case\(s\): 人写 (\d+) / 自动 (\d+) / 差分 (\d+)\)")


def counts(out):
    """验收输出 → (失败, 全部, 人写, 自动, 差分)。解析不了 → None。"""
    m = CASES.search(out)
    if not m:
        return None
    if m.group(1):
        return tuple(int(m.group(i)) for i in range(1, 6))
    return (0,) + tuple(int(m.group(i)) for i in range(6, 10))


# 这一次运行自己的工厂二进制: 同一个工作区里同时跑的验证(margin 和 accept …)不能共用会被重新编译的文件
FACTORY = os.path.join(ROOT, ".factory", f"factory-{os.getpid()}")


def build_factory():
    import atexit
    r = sh(["go", "build", "-o", FACTORY, "./factory/cmd/factory"], cwd=ROOT)
    if r.returncode:
        raise SystemExit("✗ 工厂编译失败\n" + r.stderr)
    atexit.register(lambda: os.path.exists(FACTORY) and os.remove(FACTORY))
