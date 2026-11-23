package upgrade

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"apifactory/factory/version"
)

func write(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const oldGoMod = `module github.com/x/shop/services/catalog

go 1.27.4

require (
	connectrpc.com/connect v1.21.1
	github.com/MIERAI/api-factory-runtime v0.5.2
)

replace github.com/MIERAI/api-factory-runtime => ../runtime
`

const oldMain = `package main

import (
	"fmt"

	"github.com/MIERAI/api-factory-runtime/server"
	"github.com/MIERAI/api-factory-runtime/outbox"
	"github.com/x/shop/services/catalog/gen"
)

var _ = fmt.Sprint
var _ = outbox.NotifyChannel
var _ = gen.X

func main() { server.Run(nil) }
`

func TestUpgradeFromTheOldModule(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"go.mod":             oldGoMod,
		"cmd/server/main.go": oldMain,
		"README.md":          "uses github.com/MIERAI/api-factory-runtime/server\n",
		"nested/go.mod":      "module other\n",
		"nested/x.go":        "package x\n\nimport _ \"github.com/MIERAI/api-factory-runtime/server\"\n",
		".cache/api/x.go":    "package x // github.com/MIERAI/api-factory-runtime\n",
		"internal/plain.go":  "package internal\n",
	})
	if _, err := Run(dir, Options{Check: true, NoBuild: true}); err == nil {
		t.Fatal("check passed on an old project")
	}
	res, err := Run(dir, Options{NoBuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.FromModule != version.OldRuntimeModule || res.FromVersion != "v0.5.2" || !res.Changed {
		t.Fatalf("result %+v", res)
	}
	mod := read(t, filepath.Join(dir, "go.mod"))
	if strings.Contains(mod, version.OldRuntimeModule) || !strings.Contains(mod, version.RuntimeModule+" "+version.Runtime) {
		t.Fatalf("go.mod:\n%s", mod)
	}
	if len(res.DroppedReplaces) != 1 {
		t.Fatalf("dropped replaces %v", res.DroppedReplaces)
	}
	main := read(t, filepath.Join(dir, "cmd/server/main.go"))
	if strings.Contains(main, version.OldRuntimeModule) {
		t.Fatalf("main.go:\n%s", main)
	}
	// gofmt re-sorted the import group: outbox before server
	if i, j := strings.Index(main, "/runtime/outbox"), strings.Index(main, "/runtime/server"); i < 0 || j < 0 || i > j {
		t.Fatalf("imports not sorted:\n%s", main)
	}
	if got := strings.Join(res.GoFiles, ","); got != filepath.Join("cmd", "server", "main.go") {
		t.Fatalf("Go files %q (a nested module and .cache are not the project's)", got)
	}
	if got := strings.Join(res.Mentions, ","); got != "README.md" {
		t.Fatalf("mentions %q", got)
	}
	if strings.Contains(read(t, filepath.Join(dir, "nested/x.go")), version.RuntimeModule) {
		t.Fatal("rewrote a nested module")
	}
	// idempotent; now --check passes
	res, err = Run(dir, Options{NoBuild: true})
	if err != nil || res.Changed {
		t.Fatalf("second run: %+v %v", res, err)
	}
	if _, err := Run(dir, Options{Check: true, NoBuild: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeVersionOnlyAndDowngradeGuard(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"go.mod": "module m\n\ngo 1.27.4\n\nrequire github.com/MIERAI/api-factory/runtime v0.0.1\n"})
	res, err := Run(dir, Options{NoBuild: true})
	if err != nil || !res.Changed || !strings.Contains(read(t, filepath.Join(dir, "go.mod")), version.Runtime) {
		t.Fatalf("%+v %v", res, err)
	}
	write(t, dir, map[string]string{"go.mod": "module m\n\ngo 1.27.4\n\nrequire github.com/MIERAI/api-factory/runtime v1.99.0\n"})
	if _, err := Run(dir, Options{NoBuild: true}); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("downgrade without --force: %v", err)
	}
	if _, err := Run(dir, Options{NoBuild: true, Force: true}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeNotARuntimeProject(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{"go.mod": "module m\n\ngo 1.27.4\n"})
	if _, err := Run(dir, Options{NoBuild: true}); !errors.Is(err, errNotARuntimeProject) {
		t.Fatalf("got %v", err)
	}
}
