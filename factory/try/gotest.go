package try

import (
	"bufio"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"apifactory/factory/export"
)

// 单元测试(go test -json)的结果 → 用例。测试按所在的文件分类: 从逻辑样例生成的(*_logic_examples_gen_test.go,
// 清单的 generated)= logic_example;逃生层里手写的(清单的 escape kind=test)= hand_test。其他测试(enum 的生成测试、
// 不属于任何选中 API 的手写测试)不算进 API 的用例;失败了写在 message 里。

// testEvent 是 go test -json 的一行(cmd/test2json)。
type testEvent struct {
	Action      string
	Package     string
	Test        string
	Output      string
	FailedBuild string
}

// goTestRun 是一次 go test -json 的结果。
type goTestRun struct {
	final   map[testKey]string   // 最后的 action: pass / fail / skip
	output  map[testKey][]string // 测试的输出
	order   []testKey
	buildOK bool
	build   []string // 编译失败的输出(测试编译不过)
}

type testKey struct{ Pkg, Test string }

func parseGoTest(r io.Reader) (*goTestRun, error) {
	run := &goTestRun{final: map[testKey]string{}, output: map[testKey][]string{}, buildOK: true}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		var ev testEvent
		if json.Unmarshal(line, &ev) != nil {
			// 不是 JSON 的行(go 命令本身的错误): 当作编译输出
			if s := strings.TrimSpace(string(line)); s != "" {
				run.build = append(run.build, s)
			}
			continue
		}
		switch ev.Action {
		case "build-output":
			run.build = append(run.build, strings.TrimRight(ev.Output, "\n"))
			continue
		case "build-fail":
			run.buildOK = false
			continue
		}
		if ev.Test == "" {
			if ev.Action == "fail" && ev.FailedBuild != "" {
				run.buildOK = false
			}
			if ev.Action == "output" && (strings.Contains(ev.Output, "[setup failed]") || strings.Contains(ev.Output, "[build failed]")) {
				run.buildOK = false
				run.build = append(run.build, strings.TrimRight(ev.Output, "\n"))
			}
			continue
		}
		k := testKey{ev.Package, ev.Test}
		switch ev.Action {
		case "output":
			run.output[k] = append(run.output[k], ev.Output)
		case "pass", "fail", "skip":
			if _, ok := run.final[k]; !ok {
				run.order = append(run.order, k)
			}
			run.final[k] = ev.Action
		}
	}
	return run, sc.Err()
}

// leaves: 报告的测试 = 没有子测试的测试 + 自己失败了但子测试都没失败的父测试。skip 不报告。
func (run *goTestRun) leaves() []testKey {
	parent := map[testKey]bool{}
	failedChild := map[testKey]bool{}
	for k, act := range run.final {
		name := k.Test
		for {
			i := strings.LastIndex(name, "/")
			if i < 0 {
				break
			}
			name = name[:i]
			p := testKey{k.Pkg, name}
			parent[p] = true
			if act == "fail" {
				failedChild[p] = true
			}
		}
	}
	var out []testKey
	for k, act := range run.final {
		if act == "skip" {
			continue
		}
		if !parent[k] || (act == "fail" && !failedChild[k]) {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pkg != out[j].Pkg {
			return out[i].Pkg < out[j].Pkg
		}
		return out[i].Test < out[j].Test
	})
	return out
}

// failDetail: 失败的测试输出里的第一行(=== RUN / --- FAIL 等框架的行除外)。
func (run *goTestRun) failDetail(k testKey) string {
	for _, o := range run.output[k] {
		for _, l := range strings.Split(o, "\n") {
			t := strings.TrimSpace(l)
			if t == "" || strings.HasPrefix(t, "=== ") || strings.HasPrefix(t, "--- ") || t == "FAIL" || t == "PASS" {
				continue
			}
			return t
		}
	}
	return ""
}

// owner: 一个测试文件属于哪个 API、哪种来源。
type owner struct{ API, Source string }

// testOwners: 测试文件(相对项目)→ API + 来源(只看选中的 API)。
func testOwners(m *export.Manifest, selected []string) map[string]owner {
	sel := map[string]bool{}
	for _, id := range selected {
		sel[id] = true
	}
	out := map[string]owner{}
	for _, a := range m.APIs {
		if !sel[a.ID] {
			continue
		}
		for _, g := range a.Generated {
			if strings.HasSuffix(g, "_logic_examples_gen_test.go") {
				out[g] = owner{a.ID, SourceLogicExample}
			}
		}
		for _, e := range a.Escape {
			if e.Kind == "test" && e.Exists {
				out[e.Path] = owner{a.ID, SourceHandTest}
			}
		}
	}
	return out
}

// testFuncIndex: "包的 import 路径 \x00 顶层测试函数名" → 文件(相对项目)。
type testFuncIndex map[string]string

// indexTestFuncs 读 root/dir 下所有 *_test.go 的顶层 Test 函数。
func indexTestFuncs(root, module, dir string) (testFuncIndex, error) {
	idx := testFuncIndex{}
	err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipDir
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil //nolint:nilerr // 解析不了的文件: 编译会报告
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		pkg := path.Join(module, path.Dir(rel))
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
				idx[pkg+"\x00"+fn.Name.Name] = rel
			}
		}
		return nil
	})
	return idx, err
}

// unitCases: 单元测试 → 每个选中 API 的用例;不属于选中 API 的失败的测试另外返回(包/测试名)。
func unitCases(run *goTestRun, idx testFuncIndex, owners map[string]owner) (map[string][]Case, []string) {
	per := map[string][]Case{}
	var other []string
	for _, k := range run.leaves() {
		top, _, _ := strings.Cut(k.Test, "/")
		file := idx[k.Pkg+"\x00"+top]
		passed := run.final[k] == "pass"
		o, ok := owners[file]
		if !ok {
			if !passed {
				other = append(other, k.Pkg+" "+k.Test)
			}
			continue
		}
		c := Case{Name: k.Test, Source: o.Source, Passed: passed}
		if !passed {
			c.Detail = run.failDetail(k)
		}
		per[o.API] = append(per[o.API], c)
	}
	return per, other
}
