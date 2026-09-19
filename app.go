package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"
)

//go:embed dashboard.html
var dashboard []byte

type App struct {
	lifeMu  sync.Mutex
	engine  *Engine
	cancel  context.CancelFunc
	done    chan struct{}
	trigger chan struct{}
	err     string
}

var global = &App{}

func (a *App) configure(raw []byte) error {
	cfg, err := parseConfig(raw)
	if err != nil {
		return err
	}
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()
	a.stopLocked()
	e, err := newEngine(cfg, HostBackend{call: callHost, now: time.Now}, time.Now)
	if err != nil {
		a.err = err.Error()
		return err
	}
	a.err = ""
	a.engine = e
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	done := make(chan struct{})
	trigger := make(chan struct{}, 1)
	a.done = done
	a.trigger = trigger
	go func() {
		defer close(done)
		// Startup registration precedes auth/model readiness. Never call the host inline.
		timer := time.NewTimer(time.Duration(cfg.PollSeconds) * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
				e.Tick(ctx)
			case <-timer.C:
				e.Tick(ctx)
				timer.Reset(time.Duration(cfg.PollSeconds) * time.Second)
			}
		}
	}()
	return nil
}

// Shutdown must join the worker BEFORE CPA frees the C host context and dlcloses.
// Host generation callbacks are synchronous and use CPA's normal network policy.
// Do not use a timeout-and-detach goroutine: it would call into freed C memory.
func (a *App) stopLocked() {
	if a.cancel != nil {
		a.cancel()
		<-a.done
		a.cancel = nil
		a.done = nil
	}
	if a.engine != nil {
		_ = a.engine.Close()
		a.engine = nil
	}
	a.trigger = nil
}
func (a *App) shutdown() { a.lifeMu.Lock(); defer a.lifeMu.Unlock(); a.stopLocked() }

func (a *App) handle(method string, raw []byte) ([]byte, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if json.Unmarshal(raw, &req) != nil {
			return nil, errors.New("invalid lifecycle request")
		}
		if err := a.configure(req.ConfigYAML); err != nil {
			return nil, err
		}
		return okEnvelope(registration())
	case "plugin.quiesce", "plugin.shutdown":
		a.shutdown()
		return okEnvelope(map[string]any{})
	case "management.register":
		return okEnvelope(map[string]any{"routes": []any{
			map[string]any{"Method": "GET", "Path": "/" + pluginID + "/status"},
			map[string]any{"Method": "POST", "Path": "/" + pluginID + "/check"}},
			"resources": []any{map[string]any{"Path": "/dashboard", "Menu": "5 小时自动开窗", "Description": "检测空闲额度窗口，以最低档模型开启下一轮计时；不是提前重置限额。"}}})
	case "management.handle":
		return a.management(raw)
	default:
		return nil, errors.New("unsupported plugin method")
	}
}

func registration() any {
	field := func(name, kind, desc string) any {
		return map[string]any{"Name": name, "Type": kind, "Description": desc}
	}
	return map[string]any{"schema_version": 6, "metadata": map[string]any{
		"Name": "5-Hour Window Keeper", "Version": version, "Author": "晴空 / 琴音",
		"GitHubRepository": "https://github.com/xieyuanqing/cpa-window-keeper",
		"ConfigFields": []any{
			field("dry_run", "boolean", "仅观察不发送，默认 true；关闭后自动开窗"),
			field("codex_enabled", "boolean", "监测 Codex 5 小时额度窗口"),
			field("claude_enabled", "boolean", "监测 Claude 5 小时额度窗口"),
			field("poll_seconds", "integer", "调度间隔秒数，默认 60，最小 30；活跃窗口自动减少查询"),
			field("grace_seconds", "integer", "空闲确认缓冲秒数，默认 30，最小 10"),
			field("codex_model", "string", "Codex 开窗模型，默认 gpt-5.6-luna；固定 low 思考，不自动升级大模型"),
			field("claude_model", "string", "Claude 开窗模型，默认 claude-haiku-4-5-20251001；关闭思考，max_tokens=1"),
			field("account_allowlist", "string", "可选：只处理这些 auth_index，以英文逗号分隔；留空处理所有启用的目标账号"),
			field("state_path", "string", "持久化去重文件；默认 plugins/data/cpa-window-keeper/state.json"),
		}}, "capabilities": map[string]any{"management_api": true}}
}

type managementRequest struct {
	Method, Path string
	Headers      http.Header
	Body         []byte
}
type managementResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

func response(status int, body []byte, contentType string) managementResponse {
	return managementResponse{StatusCode: status, Body: body, Headers: http.Header{"Content-Type": {contentType}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"}, "Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; frame-ancestors 'self'"}}}
}
func (a *App) management(raw []byte) ([]byte, error) {
	var req managementRequest
	if json.Unmarshal(raw, &req) != nil {
		return nil, errors.New("invalid management request")
	}
	path := strings.TrimSuffix(req.Path, "/")
	if path == "/dashboard" || path == "/v0/resource/plugins/"+pluginID+"/dashboard" {
		if req.Method != "GET" {
			return okEnvelope(response(405, []byte("method not allowed"), "text/plain"))
		}
		return okEnvelope(response(200, dashboard, "text/html; charset=utf-8"))
	}
	a.lifeMu.Lock()
	defer a.lifeMu.Unlock()
	path = strings.TrimPrefix(path, "/v0/management")
	if path == "/"+pluginID+"/status" && req.Method == "GET" {
		var status any = map[string]any{"version": version, "error": a.err, "running": false}
		if a.engine != nil {
			status = a.engine.Snapshot()
		}
		b, _ := json.Marshal(status)
		return okEnvelope(response(200, b, "application/json"))
	}
	if path == "/"+pluginID+"/check" && req.Method == "POST" {
		if a.trigger == nil {
			return okEnvelope(response(503, []byte(`{"error":"not running"}`), "application/json"))
		}
		select {
		case a.trigger <- struct{}{}:
		default:
		}
		return okEnvelope(response(202, []byte(`{"queued":true,"note":"normal quota checks and duplicate protection still apply"}`), "application/json"))
	}
	return okEnvelope(response(404, []byte("not found"), "text/plain"))
}
