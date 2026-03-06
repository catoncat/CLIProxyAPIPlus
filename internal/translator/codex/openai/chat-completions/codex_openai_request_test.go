package chat_completions

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestConvertOpenAIRequestToCodex_ServiceTierNormalization(t *testing.T) {
	tests := []struct {
		name      string
		inputTier string
		wantTier  string
		wantExist bool
	}{
		{name: "fast alias", inputTier: "fast", wantTier: "priority", wantExist: true},
		{name: "priority passthrough", inputTier: "priority", wantTier: "priority", wantExist: true},
		{name: "flex passthrough", inputTier: "flex", wantTier: "flex", wantExist: true},
		{name: "invalid removed", inputTier: "turbo", wantTier: "", wantExist: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"service_tier":"` + tt.inputTier + `"}`)
			out := ConvertOpenAIRequestToCodex("gpt-5", in, true)

			got := gjson.GetBytes(out, "service_tier")
			if got.Exists() != tt.wantExist {
				t.Fatalf("service_tier exists=%v, want %v (raw=%s)", got.Exists(), tt.wantExist, string(out))
			}
			if tt.wantExist && got.String() != tt.wantTier {
				t.Fatalf("service_tier=%q, want %q", got.String(), tt.wantTier)
			}
		})
	}
}

