package try

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 自己的 Postgres: 每次 try 一个容器(名字带进程号 + 随机数,端口由 docker 分配),开发者的 api-factory-pg 和
// 并行的 try 都不会撞。镜像和初始化参数和 Makefile 的 db-up 一样;结束时删除容器。

// PGImage: Makefile 的 PG_IMAGE(环境变量 PG_IMAGE > IMAGE_MIRROR/postgres:16-alpine)。改 Makefile 时一起改。
func PGImage() string {
	if v := os.Getenv("PG_IMAGE"); v != "" {
		return v
	}
	mirror := os.Getenv("IMAGE_MIRROR")
	if mirror == "" {
		mirror = "mirror.gcr.io/library"
	}
	return mirror + "/postgres:16-alpine"
}

type pgContainer struct {
	name string
	port string
}

func startPG(ctx context.Context, image string) (*pgContainer, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("docker not found: %w", err)
	}
	var rb [4]byte
	_, _ = rand.Read(rb[:])
	c := &pgContainer{name: fmt.Sprintf("factory-try-%d-%s", os.Getpid(), hex.EncodeToString(rb[:]))}
	args := []string{"run", "-d", "--name", c.name, "--label", "factory-try=1",
		"-e", "POSTGRES_USER=factory", "-e", "POSTGRES_PASSWORD=factory", "-e", "POSTGRES_DB=factory",
		"-e", "POSTGRES_INITDB_ARGS=--locale-provider=icu --icu-locale=en-US",
		"--tmpfs", "/var/lib/postgresql/data",
		"-p", "127.0.0.1::5432", image, "-c", "max_connections=1000", "-c", "fsync=off"}
	if out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		c.remove()
		return nil, fmt.Errorf("docker run %s: %v: %s", image, err, tail(string(out), 5))
	}
	out, err := exec.CommandContext(ctx, "docker", "port", c.name, "5432/tcp").Output()
	if err != nil {
		c.remove()
		return nil, fmt.Errorf("docker port: %w", err)
	}
	addr := strings.TrimSpace(strings.Split(string(out), "\n")[0])
	c.port = addr[strings.LastIndex(addr, ":")+1:]
	// TCP 上能连 = 初始化结束(initdb 期间的临时服务器只听 unix socket)
	deadline := time.Now().Add(120 * time.Second)
	for {
		if exec.CommandContext(ctx, "docker", "exec", c.name, "psql", "-h", "127.0.0.1", "-U", "factory", "-d", "factory", "-tAc", "select 1").Run() == nil {
			return c, nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			logs, _ := exec.Command("docker", "logs", "--tail", "20", c.name).CombinedOutput()
			c.remove()
			return nil, fmt.Errorf("postgres did not become ready: %s", tail(string(logs), 10))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (c *pgContainer) remove() {
	_ = exec.Command("docker", "rm", "-f", "-v", c.name).Run()
}

func (c *pgContainer) url(db string) string {
	return fmt.Sprintf("postgres://factory:factory@127.0.0.1:%s/%s?sslmode=disable&pool_max_conns=4", c.port, db)
}

func (c *pgContainer) psql(ctx context.Context, db, sql string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "-e", "PGOPTIONS=-c client_min_messages=warning", c.name,
		"psql", "-q", "-U", "factory", "-d", db, "-v", "ON_ERROR_STOP=1")
	cmd.Stdin = strings.NewReader(sql)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("psql %s: %v: %s", db, err, tail(out.String(), 10))
	}
	return nil
}

// worker 是一套数据库(主库 + 分库两个)。
type worker struct {
	db     string
	shards string // FACTORY_SHARDS(没有分库 = 空)
}

// setupWorkers: n 套数据库,schema = 项目的 db/schema(+ 主库加 db/views)。shards: 和 Makefile 一样,店铺 1~2 →
// 分库 a,其他 → 分库 b(工厂仓库自己的约定;其他项目和 scripts/projects.sh 一样只有主库)。
func (c *pgContainer) setupWorkers(ctx context.Context, project string, n int, shards bool) ([]worker, error) {
	schema, err := catSQL(filepath.Join(project, "db", "schema"))
	if err != nil {
		return nil, err
	}
	views, err := catSQL(filepath.Join(project, "db", "views"))
	if err != nil {
		return nil, err
	}
	type job struct{ db, sql string }
	var jobs []job
	ws := make([]worker, n)
	for i := 0; i < n; i++ {
		ws[i].db = fmt.Sprintf("try_w%d", i)
		jobs = append(jobs, job{ws[i].db, schema + views})
		if shards {
			a, b := ws[i].db+"_a", ws[i].db+"_b"
			jobs = append(jobs, job{a, schema}, job{b, schema})
			ws[i].shards = fmt.Sprintf("1-2=%s;*=%s", c.url(a), c.url(b))
		}
	}
	var create strings.Builder
	for _, j := range jobs {
		fmt.Fprintf(&create, "CREATE DATABASE %s;\n", j.db)
	}
	if err := c.psql(ctx, "factory", create.String()); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, 8)
	for i, j := range jobs {
		if j.sql == "" {
			continue
		}
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			errs[i] = c.psql(ctx, j.db, j.sql)
		}(i, j)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	return ws, nil
}

func catSQL(dir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	var b strings.Builder
	for _, f := range files {
		s, err := os.ReadFile(f) //nolint:gosec // 项目自己的 schema
		if err != nil {
			return "", err
		}
		b.Write(s)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// tail: 最后 n 行。
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
