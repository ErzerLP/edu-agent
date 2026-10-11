package config

import "testing"

func TestModelAPIStyle(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		invalid   bool
	}{
		{"", "chat_completions", false},
		{"chat_completions", "chat_completions", false},
		{"responses", "responses", false},
		{"unsupported", "", true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			values := baseEnv()
			values["MODEL_BASE_URL"] = "https://model.example.test/v1"
			values["MODEL_NAME"] = "test-model"
			values["MODEL_API_KEY"] = "test-key"
			values["MODEL_CONTEXT_WINDOW"] = "8192"
			values["MODEL_API_STYLE"] = tc.raw
			cfg, err := load(env(values))
			if tc.invalid {
				if err == nil {
					t.Fatal("invalid model API style accepted")
				}
				return
			}
			if err != nil || !cfg.Model.Enabled || cfg.Model.APIStyle != tc.want {
				t.Fatalf("incorrect model API style: %q, %v", cfg.Model.APIStyle, err)
			}
		})
	}
	values := baseEnv()
	values["MODEL_API_STYLE"] = "unsupported"
	if _, err := load(env(values)); err == nil {
		t.Fatal("invalid API style accepted with disabled model")
	}
}
