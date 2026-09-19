package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type HostCall func(method string, request any, response any) error

type HostBackend struct {
	call HostCall
	now  func() time.Time
}

func (b HostBackend) List(ctx context.Context) ([]Account, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	var r struct {
		Files []Account `json:"files"`
	}
	if err := b.call("host.auth.list", map[string]any{}, &r); err != nil {
		return nil, errors.New("host credential list unavailable")
	}
	return r.Files, nil
}

func (b HostBackend) Probe(ctx context.Context, a Account) (Quota, error) {
	if ctx.Err() != nil {
		return Quota{}, ctx.Err()
	}
	var cred struct {
		JSON struct {
			Token     string `json:"access_token"`
			AccountID string `json:"account_id"`
			Disabled  bool   `json:"disabled"`
		} `json:"json"`
	}
	if err := b.call("host.auth.get", map[string]string{"auth_index": a.Index}, &cred); err != nil {
		return Quota{}, errors.New("credential unavailable")
	}
	if cred.JSON.Token == "" || cred.JSON.Disabled {
		return Quota{}, errors.New("credential disabled or missing OAuth token")
	}
	headers := http.Header{"Authorization": {"Bearer " + cred.JSON.Token}, "Accept": {"application/json"}}
	url := ""
	switch a.Provider {
	case "codex":
		url = "https://chatgpt.com/backend-api/wham/usage"
		if cred.JSON.AccountID != "" {
			headers.Set("Chatgpt-Account-Id", cred.JSON.AccountID)
		}
	case "claude":
		url = "https://api.anthropic.com/api/oauth/usage"
		headers.Set("anthropic-beta", "oauth-2025-04-20")
		headers.Set("User-Agent", "claude-cli/2.1.5 (external, cli)")
	default:
		return Quota{}, errors.New("unsupported provider")
	}
	if ctx.Err() != nil {
		return Quota{}, ctx.Err()
	}
	var r struct {
		StatusCode int
		Body       []byte
	}
	if err := b.call("host.http.do", map[string]any{"method": "GET", "url": url, "headers": headers}, &r); err != nil {
		return Quota{}, errors.New("quota transport failed")
	}
	if r.StatusCode != 200 {
		return Quota{}, fmt.Errorf("quota HTTP %d", r.StatusCode)
	}
	if len(r.Body) > 2<<20 {
		return Quota{}, errors.New("quota response too large")
	}
	return ParseQuota(a.Provider, r.Body, b.now())
}

func (b HostBackend) Wake(ctx context.Context, a Account, c Config) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var runtime struct {
		Auth Account `json:"auth"`
	}
	if err := b.call("host.auth.get_runtime", map[string]string{"auth_index": a.Index}, &runtime); err != nil {
		return errors.New("runtime credential unavailable")
	}
	if runtime.Auth.Disabled || runtime.Auth.ID != a.ID || runtime.Auth.NextRetry.After(b.now()) {
		return errors.New("runtime credential disabled, replaced, or cooling down")
	}
	req, err := buildWakeRequest(c, a)
	if err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var r struct {
		StatusCode int             `json:"status_code"`
		Body       json.RawMessage `json:"-"`
	}
	if err = b.call("host.model.execute", req, &r); err != nil {
		return errors.New("model execution failed; no automatic immediate retry")
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return fmt.Errorf("model HTTP %d; no automatic immediate retry", r.StatusCode)
	}
	return nil
}
