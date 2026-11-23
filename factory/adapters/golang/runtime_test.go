package golang

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The pure-function gate (manifest.yaml guard.pure.forbidden_imports) must forbid the runtime module: when the runtime
// moved out of this repository (platform/ → api-factory-runtime) the old prefix stayed in the gate and nothing failed.
// Since v0.6.0 the runtime lives in this repository again (runtime/, module github.com/MIERAI/api-factory/runtime);
// projects not yet upgraded (factory upgrade) still import the old module, so the gate forbids both.
func TestPureGateForbidsTheRuntime(t *testing.T) {
	b, err := os.ReadFile("manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Guard struct {
			Pure struct {
				ForbiddenImports []string `yaml:"forbidden_imports"`
			} `yaml:"pure"`
		} `yaml:"guard"`
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{runtimeModule, oldRuntimeModule} {
		forbidden := false
		for _, imp := range m.Guard.Pure.ForbiddenImports {
			if strings.HasPrefix(`"`+mod+`/outbox"`, imp) {
				forbidden = true
			}
		}
		if !forbidden {
			t.Errorf("guard.pure.forbidden_imports %q does not forbid %q", m.Guard.Pure.ForbiddenImports, mod)
		}
	}
}

const oldRuntimeModule = "github.com/MIERAI/api-factory-runtime" // before v0.6.0 (its own repository)

// Every runtime import the adapter writes uses runtimeModule (templates and format strings spell it out): a second
// spelling would escape the gate above.
func TestRuntimeImportsUseOneModule(t *testing.T) {
	files, _ := os.ReadDir(".")
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f.Name())
		for _, line := range strings.Split(string(b), "\n") {
			if (strings.Contains(line, "api-factory-runtime") || strings.Contains(line, "api-factory/runtime")) && !strings.Contains(line, runtimeModule) {
				t.Errorf("%s: runtime import not spelled %q: %s", f.Name(), runtimeModule, strings.TrimSpace(line))
			}
		}
	}
}
