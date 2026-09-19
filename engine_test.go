package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type wkClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *wkClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *wkClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *wkClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type wkBackend struct {
	mu sync.Mutex

	accounts []Account
	listErr  error
	probeFn  func(context.Context, Account, int) (Quota, error)
	wakeFn   func(context.Context, Account, Config, int) error

	listCalls  int
	probeCalls int
	wakeCalls  int
}

func (b *wkBackend) List(ctx context.Context) ([]Account, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.listCalls++
	accounts := append([]Account(nil), b.accounts...)
	err := b.listErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return accounts, nil
}

func (b *wkBackend) Probe(ctx context.Context, a Account) (Quota, error) {
	if err := ctx.Err(); err != nil {
		return Quota{}, err
	}
	b.mu.Lock()
	b.probeCalls++
	callNo := b.probeCalls
	fn := b.probeFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, a, callNo)
	}
	return Quota{Supported: true, Idle: true}, nil
}

func (b *wkBackend) Wake(ctx context.Context, a Account, c Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	b.wakeCalls++
	callNo := b.wakeCalls
	fn := b.wakeFn
	b.mu.Unlock()
	if fn != nil {
		return fn(ctx, a, c, callNo)
	}
	return nil
}

func (b *wkBackend) counts() (list, probe, wake int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.listCalls, b.probeCalls, b.wakeCalls
}

func (b *wkBackend) wakeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wakeCalls
}

func wkBaseConfig(t *testing.T) Config {
	t.Helper()
	c := defaults()
	c.StatePath = filepath.Join(t.TempDir(), "state.json")
	c.DryRun = false
	return c
}

func wkAccount(provider, index, id string) Account {
	return Account{Provider: provider, Type: provider, Index: index, ID: id}
}

func wkNewEngine(t *testing.T, c Config, b Backend, clock *wkClock) *Engine {
	t.Helper()
	e, err := newEngine(c, b, clock.Now)
	if err != nil {
		t.Fatalf("newEngine: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func wkTick(e *Engine) {
	e.Tick(context.Background())
}

func wkState(e *Engine, index string) AccountState {
	return e.get(index)
}

func wkQuota(reset time.Time, idle bool, used float64) Quota {
	return Quota{Supported: true, ResetAt: reset, Idle: idle, UsedPercent: used}
}

func TestEngineActiveWindowDoesNotSend(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-active", "acct-active")
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return wkQuota(clock.Now().Add(4*time.Hour), false, 25), nil
		},
	}
	e := wkNewEngine(t, wkBaseConfig(t), backend, clock)

	wkTick(e)

	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("active window sent %d wake requests", got)
	}
	s := wkState(e, account.Index)
	if s.Status != "counting_down" {
		t.Fatalf("status = %q, want counting_down", s.Status)
	}
	if s.IdleSince != (time.Time{}) {
		t.Fatalf("active window set IdleSince = %v", s.IdleSince)
	}
}

func TestEngineIdleGraceRealVerificationAndFiveHourRepeat(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-repeat", "acct-repeat")
	cfg := wkBaseConfig(t)
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, callNo int) (Quota, error) {
			switch callNo {
			case 1, 2:
				return wkQuota(time.Time{}, true, 0), nil
			case 3:
				return wkQuota(clock.Now().Add(4*time.Hour), false, 1), nil
			case 4, 5:
				return wkQuota(time.Time{}, true, 0), nil
			default:
				return wkQuota(time.Time{}, true, 0), nil
			}
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)

	// The first observation starts the grace timer and must not send.
	wkTick(e)
	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("sent during the initial idle observation: %d", got)
	}

	clock.Add(time.Duration(cfg.GraceSeconds) * time.Second)
	wkTick(e)
	if got := backend.wakeCount(); got != 1 {
		t.Fatalf("wake count after grace = %d, want 1", got)
	}
	first := wkState(e, account.Index)
	if first.Attempts != 1 || !first.LastAttempt.Equal(clock.Now()) {
		t.Fatalf("reservation = attempts %d at %v, want one at %v", first.Attempts, first.LastAttempt, clock.Now())
	}
	if first.Successes != 0 {
		t.Fatalf("successful request was confirmed without a later quota query: %d", first.Successes)
	}

	// The next check is scheduled after the reservation. A real active response
	// from a later Probe is what confirms the request, not Wake returning nil.
	clock.Add(time.Duration(cfg.PollSeconds+1) * time.Second)
	wkTick(e)
	confirmed := wkState(e, account.Index)
	if confirmed.Successes != 1 || confirmed.Status != "window_started" {
		t.Fatalf("verification state = successes %d, status %q; want 1/window_started", confirmed.Successes, confirmed.Status)
	}
	if backend.wakeCount() != 1 {
		t.Fatalf("verification triggered another wake: %d", backend.wakeCount())
	}

	firstAttempt := confirmed.LastAttempt
	clock.Set(firstAttempt.Add(window))
	wkTick(e)
	if got := backend.wakeCount(); got != 1 {
		t.Fatalf("wake repeated before the five-hour reservation horizon: %d", got)
	}

	clock.Set(firstAttempt.Add(window + time.Duration(cfg.GraceSeconds)*time.Second))
	wkTick(e)
	if got := backend.wakeCount(); got != 2 {
		t.Fatalf("wake count at the next five-hour boundary = %d, want 2", got)
	}
	second := wkState(e, account.Index)
	if second.Attempts != 2 {
		t.Fatalf("attempts after second window = %d, want 2", second.Attempts)
	}

	// A second tick at the same instant cannot duplicate the reservation.
	wkTick(e)
	if got := backend.wakeCount(); got != 2 {
		t.Fatalf("same-instant tick duplicated wake: %d", got)
	}
}

func TestEngineDryRunNeverSends(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-dry", "acct-dry")
	cfg := wkBaseConfig(t)
	cfg.DryRun = true
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return wkQuota(time.Time{}, true, 0), nil
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)

	wkTick(e)
	clock.Add(time.Duration(cfg.GraceSeconds) * time.Second)
	wkTick(e)

	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("dry-run sent %d requests", got)
	}
	s := wkState(e, account.Index)
	if s.Status != "would_start_window" || s.Attempts != 0 {
		t.Fatalf("dry-run state = status %q, attempts %d; want would_start_window/0", s.Status, s.Attempts)
	}
}

func TestEngineDisabledAccountIsNotProbedOrSent(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("codex", "codex-disabled", "acct-disabled")
	account.Disabled = true
	backend := &wkBackend{accounts: []Account{account}}
	e := wkNewEngine(t, wkBaseConfig(t), backend, clock)

	wkTick(e)

	_, probes, wakes := backend.counts()
	if probes != 0 || wakes != 0 {
		t.Fatalf("disabled account calls = probes %d, wakes %d; want 0/0", probes, wakes)
	}
	s := wkState(e, account.Index)
	if s.Status != "disabled" {
		t.Fatalf("disabled status = %q, want disabled", s.Status)
	}
}

func TestEngineWeeklyBlockDoesNotSend(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-blocked", "acct-blocked")
	cfg := wkBaseConfig(t)
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return Quota{
				Supported:    true,
				Idle:         true,
				BlockReason:  "seven_day_limit",
				BlockedUntil: clock.Now().Add(5 * time.Minute),
			}, nil
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)

	wkTick(e)

	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("blocked account sent %d wake requests", got)
	}
	s := wkState(e, account.Index)
	if s.Status != "quota_blocked" || s.LastError != "seven_day_limit" {
		t.Fatalf("blocked state = status %q, error %q", s.Status, s.LastError)
	}
	wantNext := clock.Now().Add(5*time.Minute + time.Duration(cfg.GraceSeconds)*time.Second)
	if !s.NextCheck.Equal(wantNext) {
		t.Fatalf("blocked NextCheck = %v, want %v", s.NextCheck, wantNext)
	}
}

func TestEngineQuotaErrorBackoff(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-error", "acct-error")
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return Quota{}, errors.New("synthetic quota failure")
		},
	}
	e := wkNewEngine(t, wkBaseConfig(t), backend, clock)

	wkTick(e)
	s1 := wkState(e, account.Index)
	if s1.Failures != 1 || s1.Status != "quota_error" || !strings.Contains(s1.LastError, "synthetic quota failure") {
		t.Fatalf("first quota error state = %+v", s1)
	}
	if want := clock.Now().Add(5 * time.Minute); !s1.NextCheck.Equal(want) {
		t.Fatalf("first backoff = %v, want %v", s1.NextCheck, want)
	}

	clock.Add(5 * time.Minute)
	wkTick(e)
	s2 := wkState(e, account.Index)
	if s2.Failures != 2 {
		t.Fatalf("failures after retry = %d, want 2", s2.Failures)
	}
	if want := clock.Now().Add(10 * time.Minute); !s2.NextCheck.Equal(want) {
		t.Fatalf("second backoff = %v, want %v", s2.NextCheck, want)
	}
	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("quota errors sent %d wake requests", got)
	}
}

func TestEngineRequestFailureIsReservedWithoutImmediateRetry(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 6, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-failed-request", "acct-failed-request")
	cfg := wkBaseConfig(t)
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return wkQuota(time.Time{}, true, 0), nil
		},
		wakeFn: func(_ context.Context, _ Account, _ Config, _ int) error {
			return errors.New("synthetic upstream failure")
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)

	wkTick(e)
	clock.Add(time.Duration(cfg.GraceSeconds) * time.Second)
	wkTick(e)

	s := wkState(e, account.Index)
	if s.Attempts != 1 || s.Status != "request_failed_waiting_verification" {
		t.Fatalf("failed request state = attempts %d, status %q", s.Attempts, s.Status)
	}
	if !s.NextAttempt.After(clock.Now()) {
		t.Fatalf("failed request did not reserve a future retry horizon: %v", s.NextAttempt)
	}

	clock.Add(time.Duration(cfg.PollSeconds+1) * time.Second)
	wkTick(e)
	if got := backend.wakeCount(); got != 1 {
		t.Fatalf("failed request was immediately retried: %d wake calls", got)
	}
}

func TestEngineRestartPersistsReservationAndDeduplicates(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 7, 0, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "state.json")
	account := wkAccount("claude", "claude-restart", "acct-restart")
	cfg := wkBaseConfig(t)
	cfg.StatePath = path

	firstBackend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return wkQuota(time.Time{}, true, 0), nil
		},
	}
	first, err := newEngine(cfg, firstBackend, clock.Now)
	if err != nil {
		t.Fatalf("first newEngine: %v", err)
	}
	wkTick(first)
	clock.Add(time.Duration(cfg.GraceSeconds) * time.Second)
	wkTick(first)
	if got := firstBackend.wakeCount(); got != 1 {
		t.Fatalf("first engine wake count = %d, want 1", got)
	}
	reserved := wkState(first, account.Index)
	if err := first.Close(); err != nil {
		t.Fatalf("close first engine: %v", err)
	}

	clock.Add(time.Duration(cfg.PollSeconds+1) * time.Second)
	secondBackend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return wkQuota(time.Time{}, true, 0), nil
		},
	}
	second, err := newEngine(cfg, secondBackend, clock.Now)
	if err != nil {
		t.Fatalf("second newEngine: %v", err)
	}
	defer second.Close()
	wkTick(second)

	if got := secondBackend.wakeCount(); got != 0 {
		t.Fatalf("restart duplicated durable reservation: %d wake calls", got)
	}
	loaded := wkState(second, account.Index)
	if loaded.Attempts != reserved.Attempts || !loaded.LastAttempt.Equal(reserved.LastAttempt) {
		t.Fatalf("loaded reservation = attempts %d at %v, want %d at %v", loaded.Attempts, loaded.LastAttempt, reserved.Attempts, reserved.LastAttempt)
	}
}

func TestEngineConcurrentTicksDoNotDuplicateWake(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 8, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-concurrent", "acct-concurrent")
	cfg := wkBaseConfig(t)
	wakeStarted := make(chan struct{})
	wakeRelease := make(chan struct{})
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return wkQuota(time.Time{}, true, 0), nil
		},
		wakeFn: func(ctx context.Context, _ Account, _ Config, callNo int) error {
			if callNo == 1 {
				close(wakeStarted)
				select {
				case <-wakeRelease:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)
	wkTick(e)
	clock.Add(time.Duration(cfg.GraceSeconds) * time.Second)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); wkTick(e) }()
	select {
	case <-wakeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("first Tick did not reach Wake")
	}
	go func() { defer wg.Done(); wkTick(e) }()
	close(wakeRelease)
	wg.Wait()

	if got := backend.wakeCount(); got != 1 {
		t.Fatalf("concurrent ticks sent %d wake requests, want 1", got)
	}
}

func TestEngineCancellationDoesNotSend(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 9, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("claude", "claude-cancel", "acct-cancel")
	cfg := wkBaseConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, callNo int) (Quota, error) {
			if callNo == 2 {
				cancel()
			}
			return wkQuota(time.Time{}, true, 0), nil
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)

	e.Tick(ctx)
	clock.Add(time.Duration(cfg.GraceSeconds) * time.Second)
	e.Tick(ctx)

	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("cancelled Tick sent %d wake requests", got)
	}
	if got := wkState(e, account.Index).Attempts; got != 0 {
		t.Fatalf("cancelled Tick reserved %d attempts", got)
	}
}

func TestEngineMovingCodexPlaceholderNeedsTwoObservations(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("codex", "codex-moving", "acct-moving")
	cfg := wkBaseConfig(t)
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, callNo int) (Quota, error) {
			now := clock.Now()
			if callNo == 1 {
				return wkQuota(now.Add(window), false, 0), nil
			}
			return wkQuota(now.Add(window), false, 0), nil
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)

	wkTick(e)
	first := wkState(e, account.Index)
	first.NextCheck = clock.Now().Add(time.Minute)
	if !e.commit(account.Index, first) {
		t.Fatal("persisting deterministic second-observation schedule failed")
	}
	clock.Add(time.Minute)
	wkTick(e)

	moving := wkState(e, account.Index)
	if !moving.Quota.Idle {
		t.Fatalf("moving full-window 0%% placeholder remained active: %+v", moving.Quota)
	}
	if moving.Status != "idle_grace" {
		t.Fatalf("moving placeholder status = %q, want idle_grace", moving.Status)
	}
	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("moving placeholder sent before grace: %d", got)
	}
}

func TestEngineTrueActiveZeroPercentIsNotMisclassifiedAsMoving(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 11, 0, 0, 0, 0, time.UTC)}
	account := wkAccount("codex", "codex-active-zero", "acct-active-zero")
	cfg := wkBaseConfig(t)
	fixedReset := clock.Now().Add(window)
	backend := &wkBackend{
		accounts: []Account{account},
		probeFn: func(_ context.Context, _ Account, _ int) (Quota, error) {
			return wkQuota(fixedReset, false, 0), nil
		},
	}
	e := wkNewEngine(t, cfg, backend, clock)

	wkTick(e)
	first := wkState(e, account.Index)
	first.NextCheck = clock.Now().Add(time.Minute)
	if !e.commit(account.Index, first) {
		t.Fatal("persisting deterministic second-observation schedule failed")
	}
	clock.Add(time.Minute)
	wkTick(e)

	active := wkState(e, account.Index)
	if active.Quota.Idle {
		t.Fatalf("active 0%% quota was classified idle: %+v", active.Quota)
	}
	if active.Status != "counting_down" {
		t.Fatalf("active 0%% status = %q, want counting_down", active.Status)
	}
	if got := backend.wakeCount(); got != 0 {
		t.Fatalf("active 0%% sent %d wake requests", got)
	}
}

func TestEngineCrossInstanceFlockRejectsSecondOwner(t *testing.T) {
	clock := &wkClock{t: time.Date(2026, 1, 12, 0, 0, 0, 0, time.UTC)}
	path := filepath.Join(t.TempDir(), "state.json")
	cfg := wkBaseConfig(t)
	cfg.StatePath = path
	first, err := newEngine(cfg, &wkBackend{}, clock.Now)
	if err != nil {
		t.Fatalf("first newEngine: %v", err)
	}
	defer first.Close()

	if second, err := newEngine(cfg, &wkBackend{}, clock.Now); err == nil {
		_ = second.Close()
		t.Fatal("second engine acquired an already-owned state lock")
	} else if !strings.Contains(err.Error(), "another keeper owns this state path") {
		t.Fatalf("second engine error = %v, want flock refusal", err)
	}
}

func TestEngineCorruptStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not-json"), 0600); err != nil {
		t.Fatalf("write corrupt state: %v", err)
	}
	cfg := wkBaseConfig(t)
	cfg.StatePath = path
	clock := &wkClock{t: time.Date(2026, 1, 13, 0, 0, 0, 0, time.UTC)}

	if _, err := newEngine(cfg, &wkBackend{}, clock.Now); err == nil {
		t.Fatal("corrupt state was accepted and duplicate protection was reset")
	} else if !strings.Contains(err.Error(), "refusing to reset duplicate protection") {
		t.Fatalf("corrupt state error = %v, want fail-closed refusal", err)
	}
}
