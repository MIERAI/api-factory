package proj

import (
	"strings"
	"testing"

	"github.com/MIERAI/api-factory-runtime/server"
)

func TestEnvSection(t *testing.T) {
	msg := EnvSection(Uses{Messaging: true, Machines: true})
	plain := EnvSection(Uses{})
	for _, e := range server.Env { // every variable of the runtime's list appears (none hand-picked)
		if !strings.Contains(msg, "`"+e.Name+"`") {
			t.Errorf("%s missing", e.Name)
		}
	}
	if !strings.Contains(msg, "| `AMQP_URL` | **required** (sends / handles messages, state machine timeouts)") ||
		!strings.Contains(plain, "| `AMQP_URL` | not used (no messages)") {
		t.Fatalf("AMQP_URL by project:\n%s\n%s", msg, plain)
	}
	if !strings.Contains(EnvSection(Uses{Shards: true}), "| `FACTORY_SHARDS` | set it (sharded tables)") {
		t.Fatal("FACTORY_SHARDS for a sharded project")
	}
	if !strings.Contains(msg, "| `DATABASE_URL` | **required**") || !strings.Contains(msg, "Test environments only") {
		t.Fatal(msg)
	}
}

func TestWithEnvSection(t *testing.T) {
	u := Uses{Messaging: true}
	head := "# svc\n\nIntro.\n"
	once, err := WithEnvSection(head, u)
	if err != nil || !strings.HasPrefix(once, head+"\n"+envBegin) || !strings.HasSuffix(once, envEnd+"\n") {
		t.Fatalf("append: %v\n%s", err, once)
	}
	twice, _ := WithEnvSection(once, u)
	if twice != once {
		t.Fatal("not idempotent")
	}
	// replaced in place; text after the section is kept
	withTail := once + "\n## More\n"
	changed, _ := WithEnvSection(withTail, Uses{})
	if !strings.Contains(changed, "not used (no messages)") || !strings.HasSuffix(changed, "\n## More\n") || strings.Count(changed, envBegin) != 1 {
		t.Fatalf("replace:\n%s", changed)
	}
	if _, err := WithEnvSection(head+envBegin+"\n", u); err == nil {
		t.Fatal("begin without end: want an error")
	}
	if _, err := WithEnvSection(head+envEnd+"\n", u); err == nil {
		t.Fatal("end without begin: want an error")
	}
}
