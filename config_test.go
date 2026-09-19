package main

import (
	"encoding/json"
	"testing"
)

func TestConfigDefaultsAndValidation(t *testing.T) {
	c, err := parseConfig(nil)
	if err != nil || !c.DryRun || !c.Enabled || c.CodexModel != "gpt-5.6-luna" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, s := range []string{"poll_seconds: 1", "poll_seconds: 4000", "grace_seconds: 0", "state_path: ''", "codex_model: 'gpt-5.6-luna(high)'", "claude_model: gpt-5.5", "dry_run: maybe"} {
		t.Run(s, func(t *testing.T) {
			if _, err := parseConfig([]byte(s)); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestAccountAllowlistAndProviderGates(t *testing.T) {
	c := defaults()
	c.Allowlist = " abc, def "
	cases := []struct {
		a    Account
		want bool
	}{{Account{Provider: "codex", Index: "abc"}, true}, {Account{Provider: "claude", Index: "def"}, true}, {Account{Provider: "codex", Index: "xyz"}, false}, {Account{Provider: "other", Index: "abc"}, false}}
	for _, tc := range cases {
		if got := c.accepts(tc.a); got != tc.want {
			t.Fatalf("accept %+v = %v", tc.a, got)
		}
	}
	c.CodexEnabled = false
	if c.accepts(Account{Provider: "codex", Index: "abc"}) {
		t.Fatal("disabled provider accepted")
	}
}

func TestWakeRequestIsPinnedAndMinimal(t *testing.T) {
	for _, p := range []string{"codex", "claude"} {
		t.Run(p, func(t *testing.T) {
			a := Account{Provider: p, Index: "index", ID: "exact-account"}
			r, err := buildWakeRequest(defaults(), a)
			if err != nil {
				t.Fatal(err)
			}
			if r.AuthID != a.ID || r.ForcedProvider != p || r.Stream {
				t.Fatalf("unsafe routing: %+v", r)
			}
			var b map[string]any
			if err = json.Unmarshal(r.Body, &b); err != nil {
				t.Fatal(err)
			}
			if p == "codex" {
				if b["reasoning"].(map[string]any)["effort"] != "low" || len(b["tools"].([]any)) != 0 || len(b["input"].([]any)) != 1 {
					t.Fatalf("nonminimal Codex payload %s", r.Body)
				}
				if _, ok := b["max_output_tokens"]; ok {
					t.Fatal("CPA strips this unsupported output-cap knob")
				}
			} else {
				if b["thinking"].(map[string]any)["type"] != "disabled" || b["max_tokens"] != float64(1) || len(b["messages"].([]any)) != 1 {
					t.Fatalf("nonminimal Claude payload %s", r.Body)
				}
			}
		})
	}
	if _, err := buildWakeRequest(defaults(), Account{Provider: "codex"}); err == nil {
		t.Fatal("unpinned request accepted")
	}
}
