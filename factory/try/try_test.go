package try

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"apifactory/factory/export"
)

func TestParseRequest(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		paths    []string
		err      string
	}{
		{name: "empty", in: `{"schema":"factory.try-request/v1","files":[]}`, paths: []string{}},
		{name: "content and delete", in: `{"schema":"factory.try-request/v1","files":[{"path":"design/jobs/X.yaml","content":"a"},{"path":"./internal/x.go","delete":true}]}`,
			paths: []string{"design/jobs/X.yaml", "internal/x.go"}},
		{name: "empty content is a file", in: `{"schema":"factory.try-request/v1","files":[{"path":"a.txt","content":""}]}`, paths: []string{"a.txt"}},
		{name: "wrong schema", in: `{"schema":"factory.try-request/v2","files":[]}`, err: "schema must be"},
		{name: "unknown field", in: `{"schema":"factory.try-request/v1","files":[],"extra":1}`, err: "unknown field"},
		{name: "trailing data", in: `{"schema":"factory.try-request/v1","files":[]} {}`, err: "trailing"},
		{name: "neither", in: `{"schema":"factory.try-request/v1","files":[{"path":"a"}]}`, err: "is required"},
		{name: "both", in: `{"schema":"factory.try-request/v1","files":[{"path":"a","content":"x","delete":true}]}`, err: "exclusive"},
		{name: "absolute", in: `{"schema":"factory.try-request/v1","files":[{"path":"/etc/passwd","content":"x"}]}`, err: "relative"},
		{name: "escapes", in: `{"schema":"factory.try-request/v1","files":[{"path":"a/../../b","content":"x"}]}`, err: "leaves the project"},
		{name: "dot", in: `{"schema":"factory.try-request/v1","files":[{"path":".","content":"x"}]}`, err: "leaves the project"},
		{name: "git dir", in: `{"schema":"factory.try-request/v1","files":[{"path":".git/config","content":"x"}]}`, err: ".git"},
		{name: "backslash", in: `{"schema":"factory.try-request/v1","files":[{"path":"a\\b","content":"x"}]}`, err: "forward slashes"},
		{name: "twice", in: `{"schema":"factory.try-request/v1","files":[{"path":"a","content":"x"},{"path":"./a","delete":true}]}`, err: "listed twice"},
		{name: "not json", in: `nope`, err: "changes:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseRequest(strings.NewReader(tc.in))
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := req.Paths(); !reflect.DeepEqual(got, tc.paths) {
				t.Fatalf("paths = %v, want %v", got, tc.paths)
			}
		})
	}
}

func testManifest() *export.Manifest {
	loc := func(p string) export.Loc { return export.Loc{Path: p} }
	return &export.Manifest{APIs: []export.API{
		{ID: "jobs.get-job-summary", Kind: "compute", Source: loc("design/jobs/GetJobSummary.yaml"), Spec: "specs/jobs/get-job-summary/spec.yaml",
			Generated:  []string{"gen/db/jobs.get-job-summary.sql.go", "internal/handler/jobs/get_job_summary_logic_examples_gen_test.go"},
			Escape:     []export.Escape{{Loc: loc("internal/handler/jobs/get_job_summary_logic.go"), Kind: "logic", Exists: true}, {Loc: loc("internal/handler/jobs/get_job_summary_logic_test.go"), Kind: "test", Exists: true}},
			Acceptance: "gen/acceptance/jobs.get-job-summary.yaml"},
		{ID: "jobs.list-jobs", Kind: "list_keyset", Source: loc("design/jobs/ListJobs.yaml"), Spec: "specs/jobs/list-jobs/spec.yaml",
			Generated: []string{"gen/db/jobs.list-jobs.sql.go"}, Escape: []export.Escape{}},
		{ID: "sales.list-x", Kind: "custom", Source: loc("specs/sales/list-x/spec.yaml"), Spec: "specs/sales/list-x/spec.yaml",
			Escape: []export.Escape{{Loc: loc("internal/handler/sales/list_x.go"), Kind: "handler"}, {Loc: loc("internal/handler/sales/list_x_test.go"), Kind: "test"}}},
	}}
}

func TestSelectAPIs(t *testing.T) {
	all := []string{"jobs.get-job-summary", "jobs.list-jobs", "sales.list-x"}
	for _, tc := range []struct {
		name    string
		changed []string
		want    []string
	}{
		{"nothing → all", nil, all},
		{"design source", []string{"design/jobs/GetJobSummary.yaml"}, []string{"jobs.get-job-summary"}},
		{"escape logic", []string{"internal/handler/jobs/get_job_summary_logic.go"}, []string{"jobs.get-job-summary"}},
		{"generated", []string{"gen/db/jobs.list-jobs.sql.go"}, []string{"jobs.list-jobs"}},
		{"acceptance file", []string{"gen/acceptance/jobs.get-job-summary.yaml"}, []string{"jobs.get-job-summary"}},
		{"escape not written yet", []string{"internal/handler/sales/list_x.go"}, []string{"sales.list-x"}},
		{"hand-written spec", []string{"specs/sales/list-x/spec.yaml"}, []string{"sales.list-x"}},
		{"domain file → the domain", []string{"design/jobs/领域.yaml"}, []string{"jobs.get-job-summary", "jobs.list-jobs"}},
		{"unrelated → all", []string{"db/schema/jobs.sql", "README.md"}, all},
		{"two APIs, sorted, unique", []string{"design/jobs/ListJobs.yaml", "internal/handler/jobs/get_job_summary_logic_test.go", "design/jobs/ListJobs.yaml"},
			[]string{"jobs.get-job-summary", "jobs.list-jobs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := SelectAPIs(testManifest(), tc.changed); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMissingEscape(t *testing.T) {
	got := MissingEscape(testManifest())
	want := []string{"sales.list-x: internal/handler/sales/list_x.go"} // the missing _test.go does not need AI
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSuiteSource(t *testing.T) {
	for suite, want := range map[string]string{"": SourceExample, "diff": SourceDifferential, "auto:boundaries": SourceAuto, "auto:x": SourceAuto} {
		if got := SuiteSource(suite); got != want {
			t.Errorf("SuiteSource(%q) = %q, want %q", suite, got, want)
		}
	}
}

// go test -json 的输出 → 用例: 按文件分类、只报告叶子、失败的第一行。
func TestUnitCases(t *testing.T) {
	const pkg = "apifactory/internal/handler/jobs"
	stream := strings.Join([]string{
		`{"Action":"run","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples"}`,
		`{"Action":"run","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples/a_b"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples/a_b","Output":"=== RUN   Test_getJobSummaryLogic_Examples/a_b\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples/a_b","Output":"    x_test.go:12: total: want 1, got 2\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples/a_b","Output":"    x_test.go:13: second\n"}`,
		`{"Action":"fail","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples/a_b"}`,
		`{"Action":"pass","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples/c"}`,
		`{"Action":"fail","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Examples"}`,
		`{"Action":"pass","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Acceptance"}`,
		`{"Action":"skip","Package":"` + pkg + `","Test":"Test_getJobSummaryLogic_Skipped"}`,
		`{"Action":"fail","Package":"` + pkg + `","Test":"TestEnumRoundTrip"}`,
		`{"Action":"fail","Package":"` + pkg + `"}`,
	}, "\n")
	run, err := parseGoTest(strings.NewReader(stream))
	if err != nil {
		t.Fatal(err)
	}
	if !run.buildOK {
		t.Fatal("buildOK = false")
	}
	idx := testFuncIndex{
		pkg + "\x00Test_getJobSummaryLogic_Examples":   "internal/handler/jobs/get_job_summary_logic_examples_gen_test.go",
		pkg + "\x00Test_getJobSummaryLogic_Acceptance": "internal/handler/jobs/get_job_summary_logic_test.go",
		pkg + "\x00Test_getJobSummaryLogic_Skipped":    "internal/handler/jobs/get_job_summary_logic_test.go",
		pkg + "\x00TestEnumRoundTrip":                  "internal/handler/jobs/enums_gen_test.go",
	}
	per, other := unitCases(run, idx, testOwners(testManifest(), []string{"jobs.get-job-summary"}))
	want := []Case{
		{Name: "Test_getJobSummaryLogic_Acceptance", Source: SourceHandTest, Passed: true},
		{Name: "Test_getJobSummaryLogic_Examples/a_b", Source: SourceLogicExample, Passed: false, Detail: "x_test.go:12: total: want 1, got 2"},
		{Name: "Test_getJobSummaryLogic_Examples/c", Source: SourceLogicExample, Passed: true},
	}
	if !reflect.DeepEqual(per["jobs.get-job-summary"], want) {
		t.Fatalf("cases:\n got %+v\nwant %+v", per["jobs.get-job-summary"], want)
	}
	if !reflect.DeepEqual(other, []string{pkg + " TestEnumRoundTrip"}) {
		t.Fatalf("other = %v", other)
	}
	// 不选这个 API: 它的测试不算用例(失败的算 other)
	per, other = unitCases(run, idx, testOwners(testManifest(), []string{"jobs.list-jobs"}))
	if len(per) != 0 || len(other) != 2 {
		t.Fatalf("unselected: per %v other %v", per, other)
	}
}

func TestParseGoTestBuildFailure(t *testing.T) {
	for name, stream := range map[string]string{
		"build-fail action": `{"ImportPath":"x/jobs [x/jobs.test]","Action":"build-output","Output":"jobs/a_test.go:3:1: syntax error\n"}
{"ImportPath":"x/jobs [x/jobs.test]","Action":"build-fail"}
{"Action":"fail","Package":"x/jobs","FailedBuild":"x/jobs [x/jobs.test]"}`,
		"older go": `{"Action":"output","Package":"x/jobs","Output":"FAIL\tx/jobs [build failed]\n"}
{"Action":"fail","Package":"x/jobs"}`,
	} {
		t.Run(name, func(t *testing.T) {
			run, err := parseGoTest(strings.NewReader(stream))
			if err != nil {
				t.Fatal(err)
			}
			if run.buildOK || len(run.build) == 0 {
				t.Fatalf("buildOK %v build %q", run.buildOK, run.build)
			}
		})
	}
}

func TestIndexTestFuncs(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "handler", "jobs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package jobs\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\nfunc helper() {}\nfunc (x T) TestMethod() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "a_test.go"), []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	idx, err := indexTestFuncs(root, "example.com/m", "internal")
	if err != nil {
		t.Fatal(err)
	}
	want := testFuncIndex{"example.com/m/internal/handler/jobs\x00TestA": "internal/handler/jobs/a_test.go"}
	if !reflect.DeepEqual(idx, want) {
		t.Fatalf("got %v", idx)
	}
}

func TestSummarize(t *testing.T) {
	r := &Result{APIs: []APIResult{
		{ID: "a", Cases: []Case{{Source: SourceExample, Passed: true}, {Source: SourceExample}, {Source: SourceAuto, Passed: true}}},
		{ID: "b", Cases: []Case{{Source: SourceLogicExample}}},
	}}
	r.summarize()
	if r.Summary.Passed != 2 || r.Summary.Failed != 2 {
		t.Fatalf("summary %+v", r.Summary)
	}
	if c := r.Summary.BySource[SourceExample]; c.Passed != 1 || c.Failed != 1 {
		t.Fatalf("example %+v", c)
	}
	if len(r.Summary.BySource) != len(Sources) {
		t.Fatalf("by_source lists every source: %v", r.Summary.BySource)
	}
}

// 端到端(真的 docker + 本仓库): FACTORY_TEST_DB 设置时(和其他用数据库的测试一样的开关)。
// 改坏逃生层的逻辑(SLA 300 → 301 秒)→ 设计书的例子抓到。
func TestRunEndToEnd(t *testing.T) {
	if os.Getenv("FACTORY_TEST_DB") == "" || testing.Short() {
		t.Skip("FACTORY_TEST_DB not set (needs docker)")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not found")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "factory")
	build := exec.Command("go", "build", "-o", exe, "./factory/cmd/factory")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build factory: %v\n%s", err, out)
	}
	const logic = "internal/handler/jobs/get_job_summary_logic.go"
	b, err := exec.Command("git", "-C", root, "show", "HEAD:"+logic).Output()
	if err != nil {
		t.Fatal(err)
	}
	mutant := strings.Replace(string(b), "jobSummarySLASeconds = 300.0", "jobSummarySLASeconds = 301.0", 1)
	if mutant == string(b) {
		t.Skip("the SLA constant moved; update the mutant")
	}
	res, err := Run(context.Background(), Options{Exe: exe, Home: root, Project: root,
		Request: &Request{Schema: RequestSchema, Files: []File{{Path: logic, Content: &mutant}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusOK || res.Stage != StageAccept {
		t.Fatalf("status %s stage %s: %s", res.Status, res.Stage, res.Message)
	}
	if len(res.APIs) != 1 || res.APIs[0].ID != "jobs.get-job-summary" {
		t.Fatalf("apis %+v", res.APIs)
	}
	if c := res.Summary.BySource[SourceExample]; c.Failed == 0 || c.Passed == 0 {
		t.Fatalf("example cases %+v: the mutant must be caught by a design example", c)
	}
}
