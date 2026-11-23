// Package upgrade: `factory upgrade` — move a project that imports the runtime to the runtime paired with this
// factory (factory/version): the module path (github.com/MIERAI/api-factory-runtime → github.com/MIERAI/api-factory/
// runtime, v0.6.0) and the version, in go.mod and in every import of the project's Go files; then go mod tidy and go
// build ./... so a broken upgrade fails here, not in CI.
//
// What it does not do: regenerate. Generated files (`*_gen.go`, README.md, Dockerfile) come from the factory: run
// `factory --project DIR prepare` (or the project's generate script) with the same factory afterwards.
package upgrade

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"

	"apifactory/factory/version"
)

// Options of Run.
type Options struct {
	Check   bool      // change nothing; error unless the project is already at the paired runtime
	NoBuild bool      // skip go mod tidy / go build (tests; offline use)
	Force   bool      // allow moving to an older runtime than the project has
	Out     io.Writer // progress (nil: discard)
}

// Result: what Run found and changed.
type Result struct {
	FromModule, FromVersion string   // the runtime the project required before
	ToVersion               string   // = version.Runtime
	GoFiles                 []string // Go files whose imports were rewritten (relative to the project)
	DroppedReplaces         []string // replace directives of the old module (no longer meaningful)
	Mentions                []string // other files that still spell the old module (generated ones: regenerate)
	Changed                 bool
}

var errNotARuntimeProject = errors.New("go.mod does not require the runtime")

// skipDirs: never project code.
var skipDirs = map[string]bool{".git": true, "vendor": true, "node_modules": true, ".factory": true, ".cache": true, "bin": true}

// Run upgrades the project rooted at dir (its go.mod).
func Run(dir string, opt Options) (*Result, error) {
	out := opt.Out
	if out == nil {
		out = io.Discard
	}
	modPath := filepath.Join(dir, "go.mod")
	data, err := os.ReadFile(modPath)
	if err != nil {
		return nil, err
	}
	f, err := modfile.Parse(modPath, data, nil)
	if err != nil {
		return nil, err
	}
	res := &Result{ToVersion: version.Runtime}
	for _, r := range f.Require {
		if r.Mod.Path == version.RuntimeModule || (r.Mod.Path == version.OldRuntimeModule && res.FromModule == "") {
			res.FromModule, res.FromVersion = r.Mod.Path, r.Mod.Version
		}
	}
	if res.FromModule == "" {
		return nil, fmt.Errorf("%s: %w (%s or %s)", modPath, errNotARuntimeProject, version.RuntimeModule, version.OldRuntimeModule)
	}
	oldGo, err := goFilesWithOldModule(dir)
	if err != nil {
		return nil, err
	}
	upToDate := res.FromModule == version.RuntimeModule && res.FromVersion == version.Runtime && !requires(f, version.OldRuntimeModule) && len(oldGo) == 0
	if opt.Check {
		if !upToDate {
			return res, fmt.Errorf("%s: runtime %s %s, the factory pairs with %s %s (%d Go files import the old module): run factory upgrade",
				dir, res.FromModule, res.FromVersion, version.RuntimeModule, version.Runtime, len(oldGo))
		}
		return res, nil
	}
	if res.FromModule == version.RuntimeModule && semver.Compare(res.FromVersion, version.Runtime) > 0 && !opt.Force {
		return res, fmt.Errorf("%s: requires runtime %s, newer than this factory's %s (use the newer factory, or --force)", dir, res.FromVersion, version.Runtime)
	}

	// go.mod: drop the old module (require + replace), require the new one at the paired version
	if requires(f, version.OldRuntimeModule) {
		if err := f.DropRequire(version.OldRuntimeModule); err != nil {
			return nil, err
		}
	}
	for _, r := range f.Replace {
		if r.Old.Path == version.OldRuntimeModule {
			res.DroppedReplaces = append(res.DroppedReplaces, fmt.Sprintf("%s => %s %s", r.Old.Path, r.New.Path, r.New.Version))
			if err := f.DropReplace(r.Old.Path, r.Old.Version); err != nil {
				return nil, err
			}
		}
	}
	if err := f.AddRequire(version.RuntimeModule, version.Runtime); err != nil {
		return nil, err
	}
	f.Cleanup()
	newMod, err := f.Format()
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(newMod, data) {
		if err := os.WriteFile(modPath, newMod, 0o644); err != nil {
			return nil, err
		}
		res.Changed = true
		fmt.Fprintf(out, "go.mod: %s %s → %s %s\n", res.FromModule, res.FromVersion, version.RuntimeModule, version.Runtime)
	}

	// Go files: the old import path → the new one (imports re-sorted by gofmt)
	for _, rel := range oldGo {
		p := filepath.Join(dir, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		nb := bytes.ReplaceAll(b, []byte(version.OldRuntimeModule), []byte(version.RuntimeModule))
		if fb, err := format.Source(nb); err == nil {
			nb = fb
		}
		if err := os.WriteFile(p, nb, 0o644); err != nil {
			return nil, err
		}
		res.GoFiles = append(res.GoFiles, rel)
		res.Changed = true
	}
	if len(res.GoFiles) > 0 {
		fmt.Fprintf(out, "imports: %d Go files now import %s\n", len(res.GoFiles), version.RuntimeModule)
	}
	for _, r := range res.DroppedReplaces {
		fmt.Fprintf(out, "go.mod: dropped replace %s\n", r)
	}
	if res.Mentions, err = otherMentions(dir); err != nil {
		return nil, err
	}

	if !opt.NoBuild {
		for _, args := range [][]string{{"mod", "tidy"}, {"build", "./..."}} {
			fmt.Fprintf(out, "go %s\n", strings.Join(args, " "))
			cmd := exec.Command("go", args...)
			cmd.Dir = dir
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Run(); err != nil {
				return res, fmt.Errorf("%s: go %s: %w", dir, strings.Join(args, " "), err)
			}
		}
	}
	return res, nil
}

func requires(f *modfile.File, path string) bool {
	for _, r := range f.Require {
		if r.Mod.Path == path {
			return true
		}
	}
	return false
}

// walk: the project's own files (not other modules nested inside it, not tool / build directories).
func walk(dir string, fn func(rel string, d fs.DirEntry) error) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != dir {
				if skipDirs[d.Name()] {
					return filepath.SkipDir
				}
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // another module
				}
			}
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		return fn(rel, d)
	})
}

func goFilesWithOldModule(dir string) ([]string, error) {
	var out []string
	err := walk(dir, func(rel string, d fs.DirEntry) error {
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(version.OldRuntimeModule)) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

func otherMentions(dir string) ([]string, error) {
	var out []string
	err := walk(dir, func(rel string, d fs.DirEntry) error {
		if strings.HasSuffix(rel, ".go") || rel == "go.mod" || rel == "go.sum" {
			return nil
		}
		if info, err := d.Info(); err != nil || info.Size() > 1<<20 {
			return err
		}
		b, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(version.OldRuntimeModule)) {
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
