package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const pluginID = "cpa-window-keeper"
const version = "0.1.4"
const window = 5 * time.Hour

type Config struct {
	Enabled       bool   `yaml:"enabled" json:"enabled"`
	DryRun        bool   `yaml:"dry_run" json:"dry_run"`
	CodexEnabled  bool   `yaml:"codex_enabled" json:"codex_enabled"`
	ClaudeEnabled bool   `yaml:"claude_enabled" json:"claude_enabled"`
	PollSeconds   int    `yaml:"poll_seconds" json:"poll_seconds"`
	GraceSeconds  int    `yaml:"grace_seconds" json:"grace_seconds"`
	StatePath     string `yaml:"state_path" json:"-"`
	CodexModel    string `yaml:"codex_model" json:"codex_model"`
	ClaudeModel   string `yaml:"claude_model" json:"claude_model"`
	Allowlist     string `yaml:"account_allowlist" json:"account_allowlist"`
}

func defaults() Config {
	return Config{Enabled: true, DryRun: true, CodexEnabled: true, ClaudeEnabled: true,
		PollSeconds: 60, GraceSeconds: 30, StatePath: "plugins/data/cpa-window-keeper/state.json",
		CodexModel: "gpt-5.6-luna", ClaudeModel: "claude-haiku-4-5-20251001"}
}

func parseConfig(raw []byte) (Config, error) {
	c := defaults()
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return c, errors.New("invalid plugin YAML")
	}
	if c.PollSeconds < 30 || c.PollSeconds > 3600 {
		return c, errors.New("poll_seconds must be 30..3600")
	}
	if c.GraceSeconds < 10 || c.GraceSeconds > 300 {
		return c, errors.New("grace_seconds must be 10..300")
	}
	if c.StatePath == "" {
		return c, errors.New("state_path must not be empty")
	}
	model := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,100}$`)
	if !model.MatchString(c.CodexModel) || !model.MatchString(c.ClaudeModel) {
		return c, errors.New("model IDs must be plain model names, without aliases or effort suffixes")
	}
	if !strings.HasPrefix(c.CodexModel, "gpt-") || !strings.HasPrefix(c.ClaudeModel, "claude-") {
		return c, errors.New("model does not match its provider")
	}
	return c, nil
}

func (c Config) accepts(a Account) bool {
	if a.Provider != "codex" && a.Provider != "claude" {
		return false
	}
	if a.Provider == "codex" && !c.CodexEnabled || a.Provider == "claude" && !c.ClaudeEnabled {
		return false
	}
	if strings.TrimSpace(c.Allowlist) == "" {
		return true
	}
	for _, s := range strings.Split(c.Allowlist, ",") {
		if strings.TrimSpace(s) == a.Index {
			return true
		}
	}
	return false
}

// WakeRequest uses native protocols to preserve explicit thinking settings.
// AuthID and ForcedProvider prohibit scheduler fallback to a different account.
type WakeRequest struct {
	EntryProtocol  string `json:"entry_protocol"`
	ExitProtocol   string `json:"exit_protocol"`
	Model          string `json:"model"`
	Stream         bool   `json:"stream"`
	Body           []byte `json:"body"`
	ForcedProvider string `json:"forced_provider"`
	AuthID         string `json:"auth_id"`
}

func buildWakeRequest(c Config, a Account) (WakeRequest, error) {
	r := WakeRequest{AuthID: a.ID, ForcedProvider: a.Provider}
	if a.ID == "" {
		return r, errors.New("missing exact account ID")
	}
	var body map[string]any
	switch a.Provider {
	case "codex":
		r.Model = c.CodexModel
		r.EntryProtocol = "openai-response"
		r.ExitProtocol = "codex"
		body = map[string]any{"model": r.Model, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Reply OK."}}}}, "instructions": "", "reasoning": map[string]any{"effort": "low"}, "text": map[string]any{"verbosity": "low"}, "store": false, "stream": false, "tools": []any{}}
	case "claude":
		r.Model = c.ClaudeModel
		r.EntryProtocol = "claude"
		r.ExitProtocol = "claude"
		body = map[string]any{"model": r.Model, "max_tokens": 1, "messages": []any{map[string]any{"role": "user", "content": "Hi"}}, "thinking": map[string]any{"type": "disabled"}, "stream": false}
	default:
		return r, fmt.Errorf("unsupported provider")
	}
	r.Body, _ = json.Marshal(body)
	return r, nil
}
