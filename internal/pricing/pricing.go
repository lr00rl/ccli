// Package pricing turns token usage into a list-price estimate in USD.
package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/lr00rl/ccli/internal/transcript"
)

// Price is USD per million tokens. Cache writes default to 1.25x input (5
// minutes) and 2x input (1 hour) when left zero. A zero cache read defaults to
// 0.1x input.
type Price struct {
	Input        float64 `json:"input"`
	Output       float64 `json:"output"`
	CacheRead    float64 `json:"cache_read,omitempty"`
	CacheWrite5m float64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h float64 `json:"cache_write_1h,omitempty"`
	FastInput    float64 `json:"fast_input,omitempty"`
	FastOutput   float64 `json:"fast_output,omitempty"`
	// Above LongThreshold prompt tokens (input plus cache reads and writes),
	// LongInput and LongOutput replace Input and Output.
	LongThreshold int64   `json:"long_threshold,omitempty"`
	LongInput     float64 `json:"long_input,omitempty"`
	LongOutput    float64 `json:"long_output,omitempty"`
}

// Builtin holds first-party list prices: Anthropic's for Claude (Claude API
// reference, model table cached 2026-10-06) and OpenAI's for the models
// Codex runs. Cache reads are the stated exceptions to the 0.1x rule; fast
// mode is listed only where its price is published.
var Builtin = map[string]Price{
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25},
	"claude-fable-5":    {Input: 10, Output: 50},
	"claude-mythos-5":   {Input: 10, Output: 50},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.20, FastInput: 8, FastOutput: 40},
	"claude-opus-5":     {Input: 5, Output: 25, FastInput: 10, FastOutput: 50},
	"claude-opus-4-8":   {Input: 5, Output: 25},
	"claude-opus-4-7":   {Input: 5, Output: 25},
	"claude-opus-4-6":   {Input: 5, Output: 25},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.20},
	"claude-sonnet-5":   {Input: 2, Output: 10},
	"claude-sonnet-4-6": {Input: 3, Output: 15},
	"claude-haiku-5-5":  {Input: 0.10, Output: 0.50, LongThreshold: 100_000, LongInput: 0.50, LongOutput: 2.50},
	"claude-haiku-4-5":  {Input: 1, Output: 5},

	// OpenAI's list prices for the models Codex runs (API pricing page and
	// model pages, read 2026-10-16), standard tier. Input is uncached input;
	// CacheRead is cached input. "Fast" is the tier OpenAI renamed from
	// Priority. Long context starts above 272K input tokens. The pro models
	// list no cached price, so cached input costs full input.
	"gpt-5.6-sol":        {Input: 4, Output: 20, CacheRead: 0.40, FastInput: 8, FastOutput: 40, LongThreshold: 272_000, LongInput: 8, LongOutput: 30},
	"gpt-5.6-terra":      {Input: 2, Output: 12, CacheRead: 0.20, FastInput: 4, FastOutput: 24, LongThreshold: 272_000, LongInput: 4, LongOutput: 18},
	"gpt-5.6-luna":       {Input: 0.20, Output: 1.20, CacheRead: 0.02, FastInput: 0.40, FastOutput: 2.40, LongThreshold: 272_000, LongInput: 0.40, LongOutput: 1.80},
	"gpt-5.5":            {Input: 5, Output: 30, CacheRead: 0.50, FastInput: 12.50, FastOutput: 75, LongThreshold: 272_000, LongInput: 10, LongOutput: 45},
	"gpt-5.5-pro":        {Input: 30, Output: 180, CacheRead: 30, LongThreshold: 272_000, LongInput: 60, LongOutput: 270},
	"gpt-5.4":            {Input: 2.50, Output: 15, CacheRead: 0.25, FastInput: 5, FastOutput: 30, LongThreshold: 272_000, LongInput: 5, LongOutput: 22.50},
	"gpt-5.4-mini":       {Input: 0.75, Output: 4.50, CacheRead: 0.075, FastInput: 1.50, FastOutput: 9},
	"gpt-5.4-nano":       {Input: 0.20, Output: 1.25, CacheRead: 0.02},
	"gpt-5.4-pro":        {Input: 30, Output: 180, CacheRead: 30, LongThreshold: 272_000, LongInput: 60, LongOutput: 270},
	"gpt-5.3-codex":      {Input: 1.75, Output: 14, CacheRead: 0.175, FastInput: 3.50, FastOutput: 28},
	"gpt-5.2":            {Input: 1.75, Output: 14, CacheRead: 0.175, FastInput: 3.50, FastOutput: 28},
	"gpt-5.2-codex":      {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5.2-pro":        {Input: 21, Output: 168, CacheRead: 21},
	"gpt-5.1":            {Input: 1.25, Output: 10, CacheRead: 0.125, FastInput: 2.50, FastOutput: 20},
	"gpt-5.1-codex":      {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5.1-codex-max":  {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5.1-codex-mini": {Input: 0.25, Output: 2, CacheRead: 0.025},
	"gpt-5":              {Input: 1.25, Output: 10, CacheRead: 0.125, FastInput: 2.50, FastOutput: 20},
	"gpt-5-codex":        {Input: 1.25, Output: 10, CacheRead: 0.125},
	"gpt-5-mini":         {Input: 0.25, Output: 2, CacheRead: 0.025, FastInput: 0.45, FastOutput: 3.60},
	"gpt-5-nano":         {Input: 0.05, Output: 0.40, CacheRead: 0.005},
	"gpt-5-pro":          {Input: 15, Output: 120, CacheRead: 15},
}

// Table is the effective price table.
type Table struct {
	prices map[string]Price
}

// Load returns the built-in table with overrides from path merged in (a
// missing file is fine).
func Load(path string) (*Table, error) {
	t := &Table{prices: map[string]Price{}}
	for k, v := range Builtin {
		t.prices[k] = v
	}
	if path == "" {
		return t, nil
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, err
	}
	var over map[string]Price
	if err := json.Unmarshal(b, &over); err != nil {
		return nil, errors.New(path + ": " + err.Error())
	}
	for k, v := range over {
		t.prices[Normalize(k)] = v
	}
	return t, nil
}

// Hash identifies the table, so stored costs can be redone when it changes.
func (t *Table) Hash() string {
	keys := make([]string, 0, len(t.prices))
	for k := range t.prices {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	h.Write([]byte(formula))
	for _, k := range keys {
		b, _ := json.Marshal(t.prices[k])
		h.Write([]byte(k))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// Normalize maps a recorded model name to its price-table key: Claude Code
// records "claude-opus-5-5[1m]" for the 1M-context variant and some IDs carry
// a release date.
func Normalize(model string) string {
	m := strings.TrimSpace(strings.ToLower(model))
	if i := strings.IndexByte(m, '['); i > 0 {
		m = m[:i]
	}
	return dateSuffix.ReplaceAllString(m, "")
}

// formula names how Cost combines prices; it is part of Hash, so stored costs
// are recomputed when it changes.
const formula = "cost-2"

// WebSearchUSD is the list price of one server-side web search.
const WebSearchUSD = 0.01

// Weight prices an aggregate of many calls at base rates (no long-context or
// fast-mode rates, which apply per call), for splitting a known total between
// kinds of tokens. False when the model is unpriced.
func (t *Table) Weight(model string, u transcript.Usage) (float64, bool) {
	p, ok := t.prices[Normalize(model)]
	if !ok {
		return 0, false
	}
	read, w5, w1 := p.CacheRead, p.CacheWrite5m, p.CacheWrite1h
	if read == 0 {
		read = p.Input * 0.1
	}
	if w5 == 0 {
		w5 = p.Input * 1.25
	}
	if w1 == 0 {
		w1 = p.Input * 2
	}
	c := float64(u.Input)*p.Input + float64(u.Output)*p.Output + float64(u.CacheRead)*read +
		float64(u.Cache5m)*w5 + float64(u.Cache1h)*w1
	return c/1e6 + float64(u.WebSearch)*WebSearchUSD, true
}

// Priced reports whether a model has a price.
func (t *Table) Priced(model string) bool {
	_, ok := t.prices[Normalize(model)]
	return ok
}

// Cost returns the list-price cost of one response, and false when the model
// (or its fast-mode price) is unknown.
func (t *Table) Cost(model, speed string, u transcript.Usage) (float64, bool) {
	p, ok := t.prices[Normalize(model)]
	if !ok {
		return 0, false
	}
	in, out := p.Input, p.Output
	if p.LongThreshold > 0 && u.Input+u.CacheRead+u.Cache5m+u.Cache1h > p.LongThreshold {
		in, out = p.LongInput, p.LongOutput
	}
	read := p.CacheRead
	if read == 0 || in != p.Input {
		read = in * 0.1
	}
	w5, w1 := p.CacheWrite5m, p.CacheWrite1h
	if w5 == 0 || in != p.Input {
		w5 = in * 1.25
	}
	if w1 == 0 || in != p.Input {
		w1 = in * 2
	}
	if speed == "fast" {
		if p.FastInput == 0 || p.FastOutput == 0 {
			return 0, false
		}
		ratio := p.FastInput / p.Input
		in, out = p.FastInput, p.FastOutput
		read, w5, w1 = read*ratio, w5*ratio, w1*ratio
	}
	c := float64(u.Input)*in + float64(u.Output)*out + float64(u.CacheRead)*read +
		float64(u.Cache5m)*w5 + float64(u.Cache1h)*w1
	return c/1e6 + float64(u.WebSearch)*WebSearchUSD, true
}
