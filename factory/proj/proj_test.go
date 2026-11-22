package proj

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMergeOverridesDeeply(t *testing.T) {
	base := []byte("adapter: factory/adapters/golang\nproto: { package_prefix: factory, version: v1 }\nai: { models: { claude: { command: [claude] } } }\n")
	over := []byte("proto: { package_prefix: storefront }\n")
	out, err := Merge(base, over)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	p := m["proto"].(map[string]any)
	if p["package_prefix"] != "storefront" || p["version"] != "v1" {
		t.Fatalf("proto = %v: the project's key replaces, the factory's other keys stay", p)
	}
	if m["adapter"] != "factory/adapters/golang" || m["ai"] == nil {
		t.Fatalf("keys the project does not set come from the factory: %v", m)
	}
}

func TestSetupProjectAndPaths(t *testing.T) {
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd); home = "" })
	homeDir, projDir := t.TempDir(), t.TempDir()
	homeDir, _ = filepath.EvalSymlinks(homeDir) // macOS: /var → /private/var (Getwd returns the resolved path)
	os.WriteFile(filepath.Join(homeDir, "factory.yaml"), []byte("adapter: a\nproto: { package_prefix: factory, version: v1 }\n"), 0o644)
	os.WriteFile(filepath.Join(projDir, "go.mod"), []byte("module github.com/x/shop/catalog\n\ngo 1.27\n"), 0o644)
	os.WriteFile(filepath.Join(projDir, "factory.yaml"), []byte("proto: { package_prefix: shop }\n"), 0o644)
	os.Chdir(homeDir)
	if err := Setup(projDir); err != nil {
		t.Fatal(err)
	}
	if !InProject() {
		t.Fatal("InProject = false after --project")
	}
	if got := HomePath("factory/adapters/golang"); got != filepath.Join(homeDir, "factory/adapters/golang") {
		t.Fatalf("HomePath = %s", got)
	}
	if m, err := Module(); err != nil || m != "github.com/x/shop/catalog" {
		t.Fatalf("Module = %q, %v", m, err)
	}
	b, err := Config()
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Adapter string
		Proto   struct {
			PackagePrefix string `yaml:"package_prefix"`
		}
	}
	yaml.Unmarshal(b, &c)
	if c.Adapter != "a" || c.Proto.PackagePrefix != "shop" {
		t.Fatalf("Config = %+v", c)
	}
}

func TestSetupRejectsANonProject(t *testing.T) {
	wd, _ := os.Getwd()
	t.Cleanup(func() { os.Chdir(wd); home = "" })
	if err := Setup(t.TempDir()); err == nil {
		t.Fatal("a directory without go.mod is not a project")
	}
}
