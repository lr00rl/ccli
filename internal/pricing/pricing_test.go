package pricing

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/lr00rl/ccli/internal/transcript"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"claude-opus-5-5[1m]":       "claude-opus-5-5",
		"claude-haiku-4-5-20251001": "claude-haiku-4-5",
		"Claude-Sonnet-5":           "claude-sonnet-5",
		"claude-opus-5":             "claude-opus-5",
	} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCostOpus55(t *testing.T) {
	tb, _ := Load("")
	u := transcript.Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000, Cache5m: 1_000_000, Cache1h: 1_000_000}
	got, ok := tb.Cost("claude-opus-5-5[1m]", "standard", u)
	// 4 + 20 + 0.20 + 4*1.25 + 4*2
	if !ok || !near(got, 4+20+0.20+5+8) {
		t.Fatalf("cost = %v %v", got, ok)
	}
}

func TestCostDefaultsAndFast(t *testing.T) {
	tb, _ := Load("")
	u := transcript.Usage{CacheRead: 1_000_000}
	if got, _ := tb.Cost("claude-opus-5", "standard", u); !near(got, 0.5) {
		t.Fatalf("default cache read = %v", got)
	}
	u = transcript.Usage{Input: 1_000_000, Output: 1_000_000, CacheRead: 1_000_000}
	if got, ok := tb.Cost("claude-opus-5-5", "fast", u); !ok || !near(got, 8+40+0.40) {
		t.Fatalf("fast = %v %v", got, ok)
	}
	if _, ok := tb.Cost("claude-opus-4-8", "fast", u); ok {
		t.Fatal("fast without a published price should be unpriced")
	}
}

func TestHaikuLongTier(t *testing.T) {
	tb, _ := Load("")
	small := transcript.Usage{Input: 50_000, Output: 1_000_000}
	if got, _ := tb.Cost("claude-haiku-5-5", "standard", small); !near(got, 0.05*0.10+0.50) {
		t.Fatalf("small = %v", got)
	}
	big := transcript.Usage{Input: 150_000, Output: 1_000_000}
	if got, _ := tb.Cost("claude-haiku-5-5", "standard", big); !near(got, 0.15*0.50+2.50) {
		t.Fatalf("big = %v", got)
	}
}

func TestUnknownAndOverride(t *testing.T) {
	tb, _ := Load("")
	if _, ok := tb.Cost("example-local-model", "standard", transcript.Usage{Input: 10}); ok {
		t.Fatal("unknown model priced")
	}
	before := tb.Hash()
	p := filepath.Join(t.TempDir(), "prices.json")
	os.WriteFile(p, []byte(`{"kimi-k3":{"input":1,"output":2},"claude-opus-5-5[1m]":{"input":3,"output":6}}`), 0o644)
	tb2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := tb2.Cost("kimi-k3", "standard", transcript.Usage{Input: 1_000_000}); !ok || !near(got, 1) {
		t.Fatalf("override = %v %v", got, ok)
	}
	if got, _ := tb2.Cost("claude-opus-5-5", "standard", transcript.Usage{CacheRead: 1_000_000}); !near(got, 0.3) {
		t.Fatalf("override replaces builtin, cache read = %v", got)
	}
	if tb2.Hash() == before {
		t.Fatal("hash did not change")
	}
	os.WriteFile(p, []byte(`{bad`), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("bad overrides accepted")
	}
}

// OpenAI models price uncached input, cached input and output, and switch
// to long-context rates above 272K input tokens.
func TestOpenAIPrices(t *testing.T) {
	tb, _ := Load("")
	u := transcript.Usage{Input: 100_000, CacheRead: 100_000, Output: 10_000}
	if got, ok := tb.Cost("gpt-5.4", "standard", u); !ok || !near(got, 0.1*2.50+0.1*0.25+0.01*15) {
		t.Fatalf("gpt-5.4 = %v %v", got, ok)
	}
	long := transcript.Usage{Input: 200_000, CacheRead: 100_000, Output: 10_000}
	if got, _ := tb.Cost("gpt-5.4", "standard", long); !near(got, 0.2*5+0.1*0.50+0.01*22.50) {
		t.Fatalf("gpt-5.4 long = %v", got)
	}
	if got, _ := tb.Cost("gpt-5.3-codex", "fast", u); !near(got, 0.1*3.50+0.1*0.35+0.01*28) {
		t.Fatalf("gpt-5.3-codex fast = %v", got)
	}
}
