package export

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"apifactory/factory/proj"
)

// 在仓库自己的式样书上导出,检查对任何提交都成立的性质(不写死数量: 式样书会变)。
func TestBuildOnRepo(t *testing.T) {
	t.Chdir("../..")
	if err := proj.Setup(""); err != nil {
		t.Fatal(err)
	}
	m, err := Build("specs")
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema != Schema || m.Project != "." || len(m.APIs) == 0 {
		t.Fatalf("schema %q project %q apis %d", m.Schema, m.Project, len(m.APIs))
	}
	lines := map[string][]string{}
	line := func(p string, n int) string {
		if lines[p] == nil {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			lines[p] = strings.Split(string(b), "\n")
		}
		if n < 1 || n > len(lines[p]) {
			t.Fatalf("%s:%d out of range", p, n)
		}
		return lines[p][n-1]
	}
	var rules, logic, sql, logicTests int
	for _, a := range m.APIs {
		if a.SourceKind == "design" && !strings.HasPrefix(a.Source.Path, "design/") {
			t.Errorf("%s: design source %s", a.ID, a.Source.Path)
		}
		for _, r := range a.Rules {
			rules++
			if l := line(r.Path, r.Line); !strings.Contains(l, "情况") && !strings.Contains(l, "when") {
				t.Errorf("%s: rule at %s:%d is %q", a.ID, r.Path, r.Line, l)
			}
			if r.When == "" || r.Then == "" {
				t.Errorf("%s: rule at line %d without when/then", a.ID, r.Line)
			}
		}
		for _, e := range a.Examples {
			if l := line(e.Path, e.Line); !strings.HasPrefix(strings.TrimSpace(l), "-") {
				t.Errorf("%s: example at %s:%d is %q", a.ID, e.Path, e.Line, l)
			}
		}
		for _, g := range a.Generated {
			if strings.HasSuffix(g, "_logic_examples_gen_test.go") { // 头是 "from X (logic_examples). DO NOT EDIT"
				logicTests++
			}
			b, err := readHead(g)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, from := proj.Header(b); from != a.Source.Path && from != a.Spec {
				t.Errorf("%s: %s generated from %q", a.ID, g, from)
			}
		}
		for _, e := range a.Escape {
			if _, err := os.Stat(e.Path); (err == nil) != e.Exists {
				t.Errorf("%s: escape %s exists=%v", a.ID, e.Path, e.Exists)
			}
			switch e.Kind {
			case "logic":
				logic++
			case "sql":
				sql++
				if !strings.Contains(line(e.Path, e.Line), "sql:") {
					t.Errorf("%s: sql escape at %s:%d", a.ID, e.Path, e.Line)
				}
			}
		}
		if a.Kind == "compute" && !hasKind(a.Escape, "logic") {
			t.Errorf("%s: compute API without a logic escape", a.ID)
		}
	}
	if rules == 0 || logic == 0 || sql == 0 || logicTests == 0 {
		t.Fatalf("rules %d logic %d sql %d logic-example tests %d: the repo has all four", rules, logic, sql, logicTests)
	}
	again, err := Build("specs")
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(m)
	b, _ := json.Marshal(again)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("two exports of the same tree differ")
	}
}

func hasKind(es []Escape, k string) bool {
	for _, e := range es {
		if e.Kind == k {
			return true
		}
	}
	return false
}
