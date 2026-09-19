package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func copyJSON(t *testing.T, src, dst any) {
	t.Helper()
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, dst); err != nil {
		t.Fatal(err)
	}
}

func TestHostBackendPinsRuntimeAndDoesNotRetry(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	a := Account{Provider: "codex", ID: "exact-account", Index: "exact-index"}
	for _, mode := range []string{"ok", "disabled", "replaced", "cooldown", "runtime_error", "model_error", "status_error"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			b := HostBackend{now: func() time.Time { return now }, call: func(method string, req, resp any) error {
				switch method {
				case "host.auth.get_runtime":
					if mode == "runtime_error" {
						return errors.New("secret-must-not-escape")
					}
					runtime := a
					switch mode {
					case "disabled":
						runtime.Disabled = true
					case "replaced":
						runtime.ID = "other"
					case "cooldown":
						runtime.NextRetry = now.Add(time.Minute)
					}
					copyJSON(t, map[string]any{"auth": runtime}, resp)
				case "host.model.execute":
					calls++
					r := req.(WakeRequest)
					if r.AuthID != a.ID || r.ForcedProvider != "codex" {
						t.Fatal("account fallback")
					}
					if mode == "model_error" {
						return errors.New("secret-must-not-escape")
					}
					status := 200
					if mode == "status_error" {
						status = 429
					}
					copyJSON(t, map[string]any{"status_code": status}, resp)
				default:
					t.Fatal("unexpected callback", method)
				}
				return nil
			}}
			err := b.Wake(context.Background(), a, defaults())
			if (err == nil) != (mode == "ok") {
				t.Fatalf("mode %s error %v", mode, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("host error leaked")
			}
			expected := 0
			if mode == "ok" || mode == "model_error" || mode == "status_error" {
				expected = 1
			}
			if calls != expected {
				t.Fatalf("calls %d expected %d", calls, expected)
			}
		})
	}
}

func TestHostBackendQuotaUsesCorrectEndpointAndSanitizes(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			b := HostBackend{now: time.Now, call: func(method string, req, resp any) error {
				if method == "host.auth.get" {
					copyJSON(t, map[string]any{"json": map[string]any{"access_token": "test-secret", "account_id": "test-id"}}, resp)
					return nil
				}
				if method != "host.http.do" {
					t.Fatal(method)
				}
				r := req.(map[string]any)
				expected := "https://chatgpt.com/backend-api/wham/usage"
				if provider == "claude" {
					expected = "https://api.anthropic.com/api/oauth/usage"
				}
				if r["url"] != expected || r["method"] != "GET" {
					t.Fatal("wrong quota endpoint")
				}
				copyJSON(t, map[string]any{"StatusCode": 401, "Body": []byte("test-secret")}, resp)
				return nil
			}}
			_, err := b.Probe(context.Background(), Account{Provider: provider, Index: "index"})
			if err == nil || err.Error() != "quota HTTP 401" {
				t.Fatalf("unsafe error %v", err)
			}
		})
	}
}

func TestHostBackendCancelledContextDoesNotCallHost(t *testing.T) {
	b := HostBackend{now: time.Now, call: func(string, any, any) error { t.Fatal("callback after cancellation"); return nil }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.List(ctx); err == nil {
		t.Fatal("list ignored cancellation")
	}
	if _, err := b.Probe(ctx, Account{}); err == nil {
		t.Fatal("probe ignored cancellation")
	}
	if err := b.Wake(ctx, Account{}, defaults()); err == nil {
		t.Fatal("wake ignored cancellation")
	}
}
