package harness

import (
	"os"
	"strings"
	"testing"
)

// Every case here is a secret that shipped to providers verbatim before this
// existed, or an ordinary string that must survive masking untouched.
func TestMaskSecretsAssignments(t *testing.T) {
	cases := []struct{ in, want string }{
		// the canonical leak: env-style output
		{"AI_GATEWAY_API_KEY=abcdefghijklmnop end", "AI_GATEWAY_API_KEY=[redacted] end"},
		{"export OPENAI_API_KEY=sk-live-1234567890", "export OPENAI_API_KEY=[redacted]"},
		// quoting survives so the shape of the line stays readable
		{`API_KEY="double-secret"`, `API_KEY="[redacted]"`},
		{"PASSWORD='single-secret'", "PASSWORD='[redacted]'"},
		{"MY_GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuv", "MY_GITHUB_TOKEN=[redacted]"},
		// case-insensitive key match
		{"Db_Password=hunter2", "Db_Password=[redacted]"},
	}
	for _, c := range cases {
		if got := maskSecrets(c.in); got != c.want {
			t.Errorf("mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Over-masking is its own failure mode: an agent that cannot see PATH or an
// ordinary quoted value is an agent working blind. The key is what is
// sensitive, not a word appearing somewhere in the value.
func TestMaskSecretsPreservesOrdinaryOutput(t *testing.T) {
	cases := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		`PROJECT_NAME="secret-service"`,
		`GREETING='hello world'`,
		"PATH_TO_ASSETS=./assets",
		"MONKEY_HOME=/tmp/monkeys",
		"count=42 total=7",
		"tokenizer_path=/usr/share/model.tok",
	}
	for _, c := range cases {
		if got := maskSecrets(c); got != c {
			t.Errorf("ordinary output must survive: mask(%q) = %q", c, got)
		}
	}
}

// A token pasted bare, with no KEY= around it, still leaks; the well-known
// shapes are matched wherever they appear. The anchors must not eat prose.
func TestMaskSecretsTokenShapes(t *testing.T) {
	cases := []struct{ in, want string }{
		{"curl -H 'Authorization: Bearer sk-abcdefghijklmnop1234' https://api", "curl -H 'Authorization: Bearer [redacted]' https://api"},
		{"remote: https://ghp_0123456789abcdefghijklmnopqrstuvwxyz@github.com/o/r.git",
			"remote: https://[redacted]@github.com/o/r.git"},
		{"aws key AKIAIOSFODNN7EXAMPLE is live", "aws key [redacted] is live"},
		{"slack xoxb-123456789-abcdefghij", "slack [redacted]"},
		// prose that merely resembles a prefix must survive
		{"the sk-fork of the repo", "the sk-fork of the repo"},
		{"AKIA is an AWS prefix", "AKIA is an AWS prefix"},
	}
	for _, c := range cases {
		if got := maskSecrets(c.in); got != c.want {
			t.Errorf("mask(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The order inside shape is load-bearing: masking runs before the spill, so
// the file on disk holds the masked text too. Masking after would persist the
// secret; masking only the model copy leaves the read-back leaky.
func TestShapeMasksBeforeSpill(t *testing.T) {
	withSpill(t)
	got := shape("API_KEY=\"sk-abcdefghijklmnop\" then " + strings.Repeat("filler line\n", 6000))
	if strings.Contains(got, "sk-abcdefghijklmnop") {
		t.Fatal("the shaped result the model sees must not carry the secret")
	}
	if !strings.Contains(got, `API_KEY="[redacted]"`) {
		t.Fatalf("the key name and quoting must survive:\n%s", head(got, 200))
	}
	// the spill file must hold the masked text, not the original
	path, err := spill.put("sentinel")
	if err != nil {
		t.Fatal(err)
	}
	_ = path
	files, _ := os.ReadDir(spill.dir)
	for _, f := range files {
		if f.Name() == "out-1.log" {
			b, _ := os.ReadFile(spill.dir + "/out-1.log")
			if strings.Contains(string(b), "sk-abcdefghijklmnop") {
				t.Fatal("the spilled file must hold the masked text")
			}
			if !strings.Contains(string(b), "[redacted]") {
				t.Fatal("the spilled file must show the mask")
			}
		}
	}
}

// result_mask_off restores today's behavior exactly, because a masking false
// positive that breaks a legitimate workflow must be turnable off without a
// recompile.
func TestResultMaskOffRestoresRaw(t *testing.T) {
	withSpill(t)
	prev := tune.ResultMaskOff
	tune.ResultMaskOff = true
	defer func() { tune.ResultMaskOff = prev }()

	in := "API_KEY=abc123"
	if got := shape(in); got != in {
		t.Fatalf("masking off must pass output through: %q", got)
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
