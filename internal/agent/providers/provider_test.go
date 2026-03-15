package providers_test

import (
	"testing"

	"github.com/arkonis-dev/ark-operator/internal/agent/providers"
	"github.com/arkonis-dev/ark-operator/internal/agent/providers/anthropic"
)

func TestNew_Anthropic(t *testing.T) {
	for _, name := range []string{"anthropic", ""} {
		p, err := providers.New(name)
		if err != nil {
			t.Errorf("New(%q) error: %v", name, err)
		}
		if p == nil {
			t.Errorf("New(%q) returned nil", name)
		}
		if _, ok := p.(*anthropic.Provider); !ok {
			t.Errorf("New(%q) = %T, want *anthropic.Provider", name, p)
		}
	}
}

func TestNew_Unknown(t *testing.T) {
	_, err := providers.New("gemini")
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
}

func TestDetect(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"claude-sonnet-4-20250514", "anthropic"},
		{"claude-haiku-4-5-20251001", "anthropic"},
		{"gpt-4o", "openai"},
		{"gpt-4o-mini", "openai"},
		{"o1-preview", "openai"},
		{"o3-mini", "openai"},
		{"o4-mini", "openai"},
		{"unknown-model-xyz", "openai"}, // default: OpenAI-compatible (e.g. Ollama)
		{"qwen2.5:1.5b", "openai"},
		{"llama3.2", "openai"},
	}
	for _, tc := range cases {
		if got := providers.Detect(tc.model); got != tc.want {
			t.Errorf("Detect(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}
