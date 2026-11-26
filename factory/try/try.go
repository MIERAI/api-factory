package try

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/modfile"

	"apifactory/factory/export"
)

// Options: 一次 try 的输入。
type Options struct {
	Exe     string   // 工厂的可执行文件(子进程用它跑 design / prepare / export / serve-test / accept)
	Home    string   // 工厂目录(proj.Home())
	Project string   // 项目根目录(绝对路径)
	Request *Request // 改动
	APIs    []string // 只验收这些 API(空 = 改动涉及的 API;一个都不涉及 = 全部)
	Base    string   // 基准的修订(空 = HEAD)
	Keep    bool     // 不删除临时目录(调查用)
	Log     io.Writer
}

// UsageError: 调用者的用法错误(退出码 2,没有结果)。
type UsageError struct{ Err error }

func (e UsageError) Error() string { return e.Err.Error() }

// Run 执行一次 try。返回 UsageError 时没有结果;其他情况总有结果(基础设施的错误 = status error)。
func Run(ctx context.Context, o Options) (*Result, error) {
	if o.Log == nil {
		o.Log = io.Discard
	}
	if o.Base == "" {
		o.Base = "HEAD"
	}
	t := &run{o: o, res: &Result{Schema: ResultSchema, APIs: []APIResult{}}}
	t.res.summarize()
	if err := t.setup(ctx); err != nil {
		var ue UsageError
		if errors.As(err, &ue) {
			t.cleanup()
			return nil, err
		}
		t.cleanup()
		return t.fail(StatusError, StageDesign, err.Error()), nil
	}
	defer t.cleanup()
	err := t.pipeline(ctx)
	var ue UsageError
	if errors.As(err, &ue) {
		return nil, err
	}
	if err != nil {
		return t.fail(StatusError, t.res.Stage, err.Error()), nil
	}
	return t.res, nil
}

type run struct {
	o       Options
	res     *Result
	tmp     string // 临时目录(基准提交的副本)
	project string // 临时副本里的项目根目录
	home    string // 子进程的工作目录(工厂目录)
	projArg []string
	pg      *pgContainer
	pgMu    sync.Mutex
}

func (t *run) logf(format string, a ...any) { fmt.Fprintf(t.o.Log, "try: "+format+"\n", a...) }

func (t *run) fail(status, stage, msg string) *Result {
	t.res.Status, t.res.Stage, t.res.Message = status, stage, strings.TrimSpace(msg)
	t.res.summarize()
	return t.res
}

// setup: 基准提交 → 临时副本(git archive;调用者的工作区和 .git 都不动)→ 应用改动。
func (t *run) setup(ctx context.Context) error {
	top, err := gitOut(ctx, t.o.Project, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("--project %s is not in a git repository: %w", t.o.Project, err)
	}
	top, _ = filepath.EvalSymlinks(top)
	projAbs, _ := filepath.EvalSymlinks(t.o.Project)
	sha, err := gitOut(ctx, t.o.Project, "rev-parse", "--verify", "--quiet", t.o.Base+"^{commit}")
	if err != nil || sha == "" {
		return UsageError{fmt.Errorf("--base %s: not a commit", t.o.Base)}
	}
	t.res.Base = sha
	relProject, err := filepath.Rel(top, projAbs)
	if err != nil || strings.HasPrefix(relProject, "..") {
		return fmt.Errorf("project %s is outside the git repository %s", projAbs, top)
	}
	if t.tmp, err = os.MkdirTemp("", "factory-try-"); err != nil {
		return err
	}
	if t.o.Keep {
		t.logf("keep: %s", t.tmp)
	}
	arch := exec.CommandContext(ctx, "git", "-C", top, "archive", "--format=tar", sha)
	untar := exec.CommandContext(ctx, "tar", "-xf", "-", "-C", t.tmp)
	pipe, err := arch.StdoutPipe()
	if err != nil {
		return err
	}
	untar.Stdin = pipe
	var aerr, uerr bytes.Buffer
	arch.Stderr, untar.Stderr = &aerr, &uerr
	if err := untar.Start(); err != nil {
		return err
	}
	if err := arch.Run(); err != nil {
		_ = untar.Wait()
		return fmt.Errorf("git archive %s: %v: %s", sha, err, aerr.String())
	}
	if err := untar.Wait(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, uerr.String())
	}
	t.project = filepath.Join(t.tmp, relProject)
	// 工厂目录: 在同一个仓库里 → 副本里的工厂目录(基准提交的工厂资源);仓库外 → 原来的工厂目录(只读)
	homeAbs, _ := filepath.EvalSymlinks(t.o.Home)
	if rel, err := filepath.Rel(top, homeAbs); err == nil && !strings.HasPrefix(rel, "..") {
		t.home = filepath.Join(t.tmp, rel)
	} else {
		t.home = homeAbs
	}
	if filepath.Clean(t.home) != filepath.Clean(t.project) {
		t.projArg = []string{"--project", t.project}
	}
	return t.apply()
}

func (t *run) apply() error {
	for _, f := range t.o.Request.Files {
		p := filepath.Join(t.project, filepath.FromSlash(f.Path))
		if f.Delete {
			if err := os.Remove(p); err != nil {
				if os.IsNotExist(err) {
					return UsageError{fmt.Errorf("changes: delete %s: no such file at %s", f.Path, t.res.Base)}
				}
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(*f.Content), 0o644); err != nil { //nolint:gosec // 项目的源文件
			return err
		}
	}
	return nil
}

func (t *run) cleanup() {
	t.pgMu.Lock()
	if t.pg != nil {
		t.pg.remove()
		t.pg = nil
	}
	t.pgMu.Unlock()
	if t.tmp != "" && !t.o.Keep {
		_ = os.RemoveAll(t.tmp)
	}
}

func (t *run) pipeline(ctx context.Context) error {
	r := t.res
	// ① 设计: design/ → proto + specs
	r.Stage = StageDesign
	if dirs, _ := filepath.Glob(filepath.Join(t.project, "design", "*", "领域.yaml")); len(dirs) > 0 {
		start := time.Now()
		if out, err := t.factory(ctx, "design"); err != nil {
			if !isExit(err) {
				return err
			}
			t.fail(StatusGateFailed, StageDesign, gateMessage(out))
			return nil
		}
		t.logf("design ✓ %.1fs", time.Since(start).Seconds())
	}
	// 需要 AI 吗: 逃生层的主文件还没写(prepare 会写桩,所以在 prepare 之前看)
	r.Stage = StagePrepare
	pre, out, err := t.export(ctx)
	if err != nil {
		if !isExit(err) {
			return err
		}
		t.fail(StatusGateFailed, StagePrepare, gateMessage(out))
		return nil
	}
	if missing := MissingEscape(pre); len(missing) > 0 {
		t.fail(StatusNeedsAI, StagePrepare, "the escape layer needs AI (not called by try): "+strings.Join(missing, ", "))
		return nil
	}
	// ② 确定性生成(式样书闸门在里面)
	start := time.Now()
	if out, err := t.factory(ctx, "prepare"); err != nil {
		if !isExit(err) {
			return err
		}
		t.fail(StatusGateFailed, StagePrepare, gateMessage(out))
		return nil
	}
	t.logf("prepare ✓ %.1fs", time.Since(start).Seconds())
	m, out, err := t.export(ctx)
	if err != nil {
		if !isExit(err) {
			return err
		}
		t.fail(StatusGateFailed, StagePrepare, gateMessage(out))
		return nil
	}
	selected, err := t.selectAPIs(m)
	if err != nil {
		return err
	}
	for _, id := range selected {
		r.APIs = append(r.APIs, APIResult{ID: id, Cases: []Case{}})
	}
	t.logf("APIs: %s", strings.Join(selected, ", "))

	// 数据库和编译 / 单元测试同时准备
	pgDone := make(chan error, 1)
	pgCtx, cancelPG := context.WithCancel(ctx)
	go func() {
		c, err := startPG(pgCtx, PGImage())
		t.pgMu.Lock()
		t.pg = c
		t.pgMu.Unlock()
		pgDone <- err
	}()
	waitPG := func() error { err := <-pgDone; pgDone <- err; return err }
	defer func() { _ = waitPG() }() // cleanup 之前容器必须已经登记
	defer cancelPG()                // 提前结束(编译失败等): 不等数据库启动完

	// ③ 编译
	r.Stage = StageBuild
	start = time.Now()
	bin := filepath.Join(t.project, "bin", "server")
	if out, err := t.goCmd(ctx, "build", "-o", bin, "./cmd/server"); err != nil {
		if !isExit(err) {
			return err
		}
		t.fail(StatusBuildFailed, StageBuild, tail(out, 40))
		return nil
	}
	if out, err := t.goCmd(ctx, "build", "./..."); err != nil {
		if !isExit(err) {
			return err
		}
		t.fail(StatusBuildFailed, StageBuild, tail(out, 40))
		return nil
	}
	t.logf("build ✓ %.1fs", time.Since(start).Seconds())

	// ④ 单元测试
	r.Stage = StageTest
	start = time.Now()
	unit, other, buildMsg, err := t.unitTests(ctx, m, selected)
	if err != nil {
		return err
	}
	if buildMsg != "" {
		t.fail(StatusBuildFailed, StageTest, buildMsg)
		return nil
	}
	for i := range r.APIs {
		r.APIs[i].Cases = append(r.APIs[i].Cases, unit[r.APIs[i].ID]...)
	}
	t.logf("unit tests ✓ %.1fs", time.Since(start).Seconds())

	// ⑤ 验收
	r.Stage = StageAccept
	if err := waitPG(); err != nil {
		return err
	}
	start = time.Now()
	if err := t.accept(ctx, m, selected); err != nil {
		return err
	}
	t.logf("accept ✓ %.1fs", time.Since(start).Seconds())

	r.Status = StatusOK
	var notes []string
	if len(other) > 0 {
		notes = append(notes, fmt.Sprintf("%d unit test(s) outside the selected APIs failed: %s", len(other), strings.Join(other, "; ")))
	}
	for _, a := range r.APIs {
		if a.Error != "" {
			notes = append(notes, fmt.Sprintf("%s: acceptance did not run: %s", a.ID, firstLine(a.Error)))
		}
	}
	r.Message = strings.Join(notes, "\n")
	r.summarize()
	return nil
}

// selectAPIs: --api(必须存在)或者改动涉及的 API。
func (t *run) selectAPIs(m *export.Manifest) ([]string, error) {
	if len(t.o.APIs) == 0 {
		return SelectAPIs(m, t.o.Request.Paths()), nil
	}
	known := map[string]bool{}
	for _, a := range m.APIs {
		known[a.ID] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, id := range t.o.APIs {
		if !known[id] {
			return nil, UsageError{fmt.Errorf("--api %s: no such API (see factory export)", id)}
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// factory: 在临时副本里跑工厂的命令。
func (t *run) factory(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, t.o.Exe, append(append([]string{}, t.projArg...), args...)...)
	cmd.Dir = t.home
	cmd.Env = cleanEnv(nil)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

func (t *run) export(ctx context.Context) (*export.Manifest, string, error) {
	f := filepath.Join(t.tmp, ".try-manifest.json")
	out, err := t.factory(ctx, "export", "--out", f)
	if err != nil {
		return nil, out, err
	}
	b, err := os.ReadFile(f) //nolint:gosec // 自己写的文件
	if err != nil {
		return nil, out, err
	}
	var m export.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, out, err
	}
	return &m, out, nil
}

func (t *run) goCmd(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = t.project
	cmd.Env = cleanEnv(nil)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// unitTests: go test -json ./internal/... → 选中 API 的用例 / 其他失败的测试 / 测试编译失败的信息。
func (t *run) unitTests(ctx context.Context, m *export.Manifest, selected []string) (map[string][]Case, []string, string, error) {
	if _, err := os.Stat(filepath.Join(t.project, "internal")); err != nil {
		return nil, nil, "", nil //nolint:nilerr // 没有 internal/: 没有单元测试
	}
	gomod, err := os.ReadFile(filepath.Join(t.project, "go.mod"))
	if err != nil {
		return nil, nil, "", err
	}
	module := modfile.ModulePath(gomod)
	idx, err := indexTestFuncs(t.project, module, "internal")
	if err != nil {
		return nil, nil, "", err
	}
	cmd := exec.CommandContext(ctx, "go", "test", "-count=1", "-json", "./internal/...")
	cmd.Dir = t.project
	cmd.Env = cleanEnv(nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	gr, err := parseGoTest(&stdout)
	if err != nil {
		return nil, nil, "", err
	}
	if !gr.buildOK || (runErr != nil && len(gr.final) == 0) {
		msg := strings.Join(gr.build, "\n")
		if strings.TrimSpace(msg) == "" {
			msg = stderr.String()
		}
		return nil, nil, "unit tests do not compile:\n" + tail(msg, 40), nil
	}
	per, other := unitCases(gr, idx, testOwners(m, selected))
	return per, other, "", nil
}

// accept: 选中的 API 并行验收(每个 worker 一套数据库;每个 API 一次 serve-test,和 scripts/accept-par.py 一样)。
func (t *run) accept(ctx context.Context, m *export.Manifest, selected []string) error {
	specDir := map[string]string{}
	for _, a := range m.APIs {
		specDir[a.ID] = path.Dir(a.Spec)
	}
	n := min(len(selected), max(1, runtime.NumCPU()))
	if n == 0 {
		return nil
	}
	shards := len(t.projArg) == 0 // 工厂仓库自己(Makefile 的分库约定);其他项目只有主库(scripts/projects.sh)
	t.pgMu.Lock()
	pg := t.pg
	t.pgMu.Unlock()
	ws, err := pg.setupWorkers(ctx, t.project, n, shards)
	if err != nil {
		return err
	}
	pool := make(chan worker, n)
	for _, w := range ws {
		pool <- w
	}
	bin := filepath.Join(t.project, "bin", "server")
	idx := map[string]int{}
	for i, a := range t.res.APIs {
		idx[a.ID] = i
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, id := range selected {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			w := <-pool
			defer func() { pool <- w }()
			cases, errMsg := t.acceptOne(ctx, pg, w, bin, specDir[id], i)
			mu.Lock()
			a := &t.res.APIs[idx[id]]
			a.Cases = append(cases, a.Cases...) // 验收在前,单元测试在后
			a.Error = errMsg
			mu.Unlock()
		}(i, id)
	}
	wg.Wait()
	return ctx.Err()
}

func (t *run) acceptOne(ctx context.Context, pg *pgContainer, w worker, bin, specDir string, i int) ([]Case, string) {
	url := pg.url(w.db)
	results := filepath.Join(t.tmp, fmt.Sprintf(".try-accept-%d.json", i))
	inner := []string{t.o.Exe}
	if len(t.projArg) > 0 {
		inner = append(inner, "--project", ".") // serve-test 在项目目录里执行命令(scripts/projects.sh 一样)
	}
	inner = append(inner, "accept", "--db", url, "--results", results, specDir)
	args := append(append([]string{}, t.projArg...), "serve-test", "--server", bin, "--db", url, "--")
	cmd := exec.CommandContext(ctx, t.o.Exe, append(args, inner...)...)
	cmd.Dir = t.home
	extra := []string{"GOGC=400"}
	if w.shards != "" {
		extra = append(extra, "FACTORY_SHARDS="+w.shards)
	}
	cmd.Env = cleanEnv(extra)
	out, runErr := cmd.CombinedOutput()
	b, err := os.ReadFile(results) //nolint:gosec // 自己写的文件
	if err != nil {
		msg := tail(string(out), 10)
		if runErr != nil && msg == "" {
			msg = runErr.Error()
		}
		return []Case{}, "acceptance runner: " + msg
	}
	var rows []struct {
		Suite  string `json:"suite"`
		Case   string `json:"case"`
		Passed bool   `json:"passed"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(b, &rows); err != nil {
		return []Case{}, "acceptance results: " + err.Error()
	}
	cases := make([]Case, 0, len(rows))
	for _, r := range rows {
		c := Case{Name: r.Case, Source: SuiteSource(r.Suite), Passed: r.Passed}
		if !r.Passed {
			c.Detail = firstLine(r.Detail)
		}
		cases = append(cases, c)
	}
	return cases, ""
}

// cleanEnv: 调用者环境里指向开发者的数据库 / 消息 / 服务器的变量去掉(try 只用自己的)。
func cleanEnv(extra []string) []string {
	drop := map[string]bool{"DATABASE_URL": true, "BASE_URL": true, "FACTORY_SHARDS": true, "AMQP_URL": true,
		"FACTORY_TEST_AMQP": true, "API_TOKEN": true, "AUTH_MODE": true, "FACTORY_DIFF_SEED": true,
		"FACTORY_ACCEPT_STOP_AFTER": true, "FACTORY_TEST_DB": true}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	return append(env, extra...)
}

// gateMessage: 闸门的输出里的问题(通过的行 "✓ …" 去掉),最后 40 行。
func gateMessage(out string) string {
	var keep []string
	for _, l := range strings.Split(out, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "✓") {
			keep = append(keep, l)
		}
	}
	return tail(strings.Join(keep, "\n"), 40)
}

// isExit: 命令执行了但失败了(闸门 / 编译的失败);否则是基础设施的错误(命令不存在、被取消等)。
func isExit(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee)
}

func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	b, err := cmd.Output()
	return strings.TrimSpace(string(b)), err
}
