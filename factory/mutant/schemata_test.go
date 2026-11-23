package mutant

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const orig = `package p

import "strings"

const q = ` + "`SELECT 1 < 2`" + `

type T struct{ n int }

func (t *T) Add(a, b int, rest ...int) int {
	s := a + b + t.n
	for _, r := range rest {
		s += r
	}
	return s
}

func Up(s string) string { return strings.ToUpper(s) }

func Noop(s string) {}

func Q() string { return q }
`

// 合编后: 不设 FACTORY_MUTANT = 原来的行为;设了 = 那个变异的行为(和单独编译一样)。
func TestSchemataSelectsMutant(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(root, "go.mod"), []byte("module m\n\ngo 1.22\n"), 0o644))
	must(os.MkdirAll(filepath.Join(root, "p"), 0o755))
	must(os.WriteFile(filepath.Join(root, "p", "p.go"), []byte(orig), 0o644))
	mut := func(id, from, to string) Mutant {
		if !strings.Contains(orig, from) {
			t.Fatalf("%s: %q not in source", id, from)
		}
		f := filepath.Join(out, id+".go")
		must(os.WriteFile(f, []byte(strings.Replace(orig, from, to, 1)), 0o644))
		return Mutant{ID: id, Path: "p/p.go", Mutated: f}
	}
	muts := []Mutant{
		mut("1", "SELECT 1 < 2", "SELECT 1 <= 2"),
		mut("2", "s := a + b + t.n", "s := a - b + t.n"),
		mut("3", "strings.ToUpper(s)", "strings.ToLower(s)"),
		mut("4", "s += r", "s -= r"),
		mut("5", `import "strings"`, `import "strings" // x`),               // 声明之外 → 单独编译
		mut("6", "func Noop(s string) {}", "func Noop(s string) { _ = s }"), // 没有返回值
	}
	res, err := Build(root, out, muts)
	must(err)
	if strings.Join(res.Schemata, ",") != "1,2,3,4,6" || res.Fallback["5"] == "" {
		t.Fatalf("schemata %v fallback %v", res.Schemata, res.Fallback)
	}
	must(os.WriteFile(filepath.Join(root, "main.go"), []byte(`package main

import ("fmt"; "m/p")

func main() { t := &p.T{}; fmt.Println(p.Q(), t.Add(5, 2, 1), p.Up("Ab")); p.Noop("") }
`), 0o644))
	bin := filepath.Join(out, "bin")
	cmd := exec.Command("go", "build", "-overlay", res.Overlay, "-o", bin, ".")
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, b)
	}
	for id, want := range map[string]string{
		"":  "SELECT 1 < 2 8 AB",
		"1": "SELECT 1 <= 2 8 AB",
		"2": "SELECT 1 < 2 4 AB",
		"3": "SELECT 1 < 2 8 ab",
		"4": "SELECT 1 < 2 6 AB",
		"6": "SELECT 1 < 2 8 AB",
	} {
		c := exec.Command(bin)
		c.Env = append(os.Environ(), "FACTORY_MUTANT="+id)
		b, err := c.Output()
		must(err)
		if got := strings.TrimSpace(string(b)); got != want {
			t.Errorf("FACTORY_MUTANT=%q: got %q, want %q", id, got, want)
		}
	}
}
