package system

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const sample = "../../examples/storefront-v2"

// copySystem: the storefront v2 sample's inputs (factory.yaml, design/*/消息.yaml, specs/, contracts/) in a temp dir.
func copySystem(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(sample, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(sample, path)
		keep := strings.HasPrefix(rel, "contracts") || strings.HasSuffix(rel, "factory.yaml") ||
			strings.Contains(rel, string(filepath.Separator)+"specs") || strings.HasSuffix(rel, "消息.yaml")
		if d.IsDir() || !keep {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func mustLoad(t *testing.T, dir string) *System {
	t.Helper()
	s, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func expectProblem(t *testing.T, s *System, want string) {
	t.Helper()
	ps := s.Check()
	for _, p := range ps {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("want a problem containing %q, got %q", want, ps)
}

func editJSON(t *testing.T, path string, edit func(doc map[string]any)) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	b, _ = json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// The sample passes, its manifest on disk is up to date, and the manifest's queues are exactly the queues the generated
// wirings consume and declare (one rule for the queue names: <service>.<type>).
func TestSampleSystem(t *testing.T) {
	s := mustLoad(t, sample)
	if ps := s.Check(); len(ps) > 0 {
		t.Fatalf("the sample's contracts disagree: %q", ps)
	}
	b, err := s.ManifestJSON()
	if err != nil {
		t.Fatal(err)
	}
	disk, err := os.ReadFile(filepath.Join(sample, "contracts", ManifestFile))
	if err != nil || string(disk) != string(b) {
		t.Fatalf("%s/contracts/%s is not up to date (factory system %s)", sample, ManifestFile, sample)
	}
	m, _ := s.Manifest()
	var got []string
	for _, q := range m.Queues {
		got = append(got, q.Name)
	}
	wired, _ := filepath.Glob(filepath.Join(sample, "*", "cmd", "server", "wiring_gen.go"))
	re := regexp.MustCompile(`consume\.Consumer\{Queue: "([^"]+)"`)
	var want []string
	for _, w := range wired {
		b, _ := os.ReadFile(w)
		for _, x := range re.FindAllStringSubmatch(string(b), -1) {
			want = append(want, x[1])
		}
	}
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("manifest queues\n  %v\nwirings\n  %v", got, want)
	}
}

func TestSystemFindsProblems(t *testing.T) {
	t.Run("no handler for an event", func(t *testing.T) {
		dir := copySystem(t)
		os.RemoveAll(filepath.Join(dir, "notification"))
		expectProblem(t, mustLoad(t, dir), "order.confirmed (event): sent by order(order.order-saga) but nobody in the system handles it")
	})
	t.Run("外部 lifts the counterpart rule", func(t *testing.T) {
		dir := copySystem(t)
		os.RemoveAll(filepath.Join(dir, "notification"))
		for _, f := range []string{"order.confirmed.v1.json", "order.cancelled.v1.json"} {
			editJSON(t, filepath.Join(dir, "contracts", f), func(doc map[string]any) { doc["x-factory"].(map[string]any)["external"] = true })
		}
		if ps := mustLoad(t, dir).Check(); len(ps) > 0 {
			t.Fatalf("external events need no handler in the system: %q", ps)
		}
	})
	t.Run("nobody sends a command", func(t *testing.T) {
		dir := copySystem(t)
		os.RemoveAll(filepath.Join(dir, "order"))
		expectProblem(t, mustLoad(t, dir), "inventory.reserve (command): handled by inventory(inventory.reserve-stock) but nobody in the system sends it")
	})
	t.Run("contract changed, project not regenerated", func(t *testing.T) {
		dir := copySystem(t)
		editJSON(t, filepath.Join(dir, "contracts", "payment.charge.v1.json"), func(doc map[string]any) {
			x := doc["x-factory"].(map[string]any)
			fields := x["fields"].([]any)
			x["fields"] = append(fields, map[string]any{"name": "currency", "type": "文本"})
		})
		expectProblem(t, mustLoad(t, dir), "payment.charge v1: payment(payment.charge-payment) was generated with other fields than the contract")
	})
	t.Run("new version published, the other side still on v1", func(t *testing.T) {
		dir := copySystem(t)
		src := filepath.Join(dir, "contracts", "payment.succeeded.v1.json")
		b, _ := os.ReadFile(src)
		v2 := filepath.Join(dir, "contracts", "payment.succeeded.v2.json")
		os.WriteFile(v2, b, 0o644)
		editJSON(t, v2, func(doc map[string]any) { doc["x-factory"].(map[string]any)["version"] = 2 })
		expectProblem(t, mustLoad(t, dir), "payment.succeeded: order(order.order-saga) accepts v1, the contract is v2")
	})
	t.Run("a command with two handlers", func(t *testing.T) {
		dir := copySystem(t)
		src := filepath.Join(dir, "inventory", "specs", "inventory", "reserve-stock")
		dst := filepath.Join(dir, "payment", "specs", "payment", "reserve-stock")
		os.MkdirAll(dst, 0o755)
		b, _ := os.ReadFile(filepath.Join(src, "spec.yaml"))
		os.WriteFile(filepath.Join(dst, "spec.yaml"), []byte(strings.ReplaceAll(string(b), "inventory.inventory.v1", "payment.payment.v1")), 0o644)
		s := mustLoad(t, dir)
		expectProblem(t, s, "inventory.reserve (command): handled by inventory, payment; a command has exactly one handler")
		expectProblem(t, s, "handled by payment(payment.reserve-stock), but inventory owns it")
	})
	t.Run("projects disagree on the contracts directory", func(t *testing.T) {
		dir := copySystem(t)
		os.WriteFile(filepath.Join(dir, "payment", "factory.yaml"), []byte("proto:\n  package_prefix: payment\ncontracts: elsewhere\n"), 0o644)
		if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "shares one contracts directory") {
			t.Fatalf("want the shared-directory error, got %v", err)
		}
	})
}
