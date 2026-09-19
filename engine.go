package main

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

type Account struct {
	ID        string    `json:"id"`
	Index     string    `json:"auth_index"`
	Provider  string    `json:"provider"`
	Type      string    `json:"type"`
	Disabled  bool      `json:"disabled"`
	NextRetry time.Time `json:"next_retry_after"`
}

type Backend interface {
	List(context.Context) ([]Account, error)
	Probe(context.Context, Account) (Quota, error)
	Wake(context.Context, Account, Config) error
}

type Engine struct {
	cfg       Config
	backend   Backend
	store     *Store
	now       func() time.Time
	tickMu    sync.Mutex
	mu        sync.Mutex
	state     State
	lastError string
	fatal     bool
}

func newEngine(c Config, b Backend, now func() time.Time) (*Engine, error) {
	s, state, err := openStore(c.StatePath)
	if err != nil {
		return nil, err
	}
	return &Engine{cfg: c, backend: b, store: s, state: state, now: now}, nil
}

func (e *Engine) commit(index string, s AccountState) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state.Accounts[index] = &s
	if err := e.store.Save(e.state); err != nil {
		e.lastError = err.Error()
		e.fatal = true
		return false
	}
	return true
}

func (e *Engine) get(index string) AccountState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.state.Accounts[index]; s != nil {
		return *s
	}
	return AccountState{}
}

func (e *Engine) Snapshot() any {
	e.mu.Lock()
	defer e.mu.Unlock()
	// Detach maps from the mutable scheduler before the management handler marshals them.
	raw, _ := json.Marshal(e.state)
	var state State
	_ = json.Unmarshal(raw, &state)
	return map[string]any{"version": version, "config": e.cfg, "state": state, "error": e.lastError, "sending_suspended": e.fatal, "server_time": e.now().UTC()}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (e *Engine) Tick(ctx context.Context) {
	// Prevent manual checks, timer ticks, and reconfiguration from overlapping.
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	e.mu.Lock()
	fatal := e.fatal
	e.mu.Unlock()
	if fatal || !e.cfg.Enabled || ctx.Err() != nil {
		return
	}
	accounts, err := e.backend.List(ctx)
	if err != nil {
		e.mu.Lock()
		e.lastError = "credential list failed"
		e.mu.Unlock()
		return
	}
	e.mu.Lock()
	e.lastError = ""
	e.mu.Unlock()
	for _, a := range accounts {
		if ctx.Err() != nil {
			return
		}
		if a.Provider == "" {
			a.Provider = a.Type
		}
		if a.Index == "" || a.ID == "" || !e.cfg.accepts(a) {
			continue
		}
		s := e.get(a.Index)
		s.Provider = a.Provider
		now := e.now().UTC()
		if a.Disabled {
			s.Status = "disabled"
			s.IdleSince = time.Time{}
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		if a.NextRetry.After(now) {
			s.Status = "host_cooldown"
			s.NextCheck = minTime(a.NextRetry, now.Add(15*time.Minute))
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		if s.NextCheck.After(now) {
			continue
		}
		q, err := e.backend.Probe(ctx, a)
		now = e.now().UTC()
		s.LastCheck = now
		if err != nil {
			s.Failures++
			s.Status = "quota_error"
			s.LastError = err.Error()
			s.IdleSince = time.Time{}
			delay := 5 * time.Minute * time.Duration(1<<min(s.Failures-1, 3))
			s.NextCheck = now.Add(delay)
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		s.Failures = 0
		s.LastError = ""
		s.Quota = q
		// A never-started Codex window may be a full-length moving placeholder.
		// Two zero-use observations must move forward with time, unlike an active 0%-rounded window.
		elapsed := now.Sub(s.PreviousCheck)
		shift := q.ResetAt.Sub(s.PreviousReset)
		moving := a.Provider == "codex" && q.Supported && q.UsedPercent == 0 && s.PreviousUsed == 0 &&
			!s.PreviousCheck.IsZero() && elapsed >= 30*time.Second && elapsed <= 10*time.Minute &&
			absDuration(q.ResetAt.Sub(now)-window) <= 5*time.Second &&
			absDuration(s.PreviousReset.Sub(s.PreviousCheck)-window) <= 5*time.Second &&
			absDuration(shift-elapsed) <= 5*time.Second
		stable := !s.PreviousCheck.IsZero() && elapsed >= 30*time.Second && absDuration(shift) <= 5*time.Second
		s.PreviousCheck = now
		s.PreviousReset = q.ResetAt
		s.PreviousUsed = q.UsedPercent
		if moving {
			q.Idle = true
			s.Quota = q
		}
		if !q.Supported {
			s.Status = "no_five_hour_window"
			s.NextCheck = now.Add(6 * time.Hour)
			s.IdleSince = time.Time{}
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		if q.BlockReason != "" {
			s.Status = "quota_blocked"
			s.LastError = q.BlockReason
			s.IdleSince = time.Time{}
			s.NextCheck = now.Add(15 * time.Minute)
			if q.BlockedUntil.After(now) {
				s.NextCheck = minTime(s.NextCheck, q.BlockedUntil.Add(time.Duration(e.cfg.GraceSeconds)*time.Second))
			}
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		if !q.Idle {
			s.IdleSince = time.Time{}
			s.Status = "counting_down"
			s.NextCheck = minTime(now.Add(15*time.Minute), q.ResetAt.Add(time.Duration(e.cfg.GraceSeconds)*time.Second))
			if s.NextCheck.Before(now) {
				s.NextCheck = now.Add(time.Duration(e.cfg.PollSeconds) * time.Second)
			}
			if q.UsedPercent == 0 && !stable {
				s.NextCheck = now.Add(time.Duration(e.cfg.PollSeconds) * time.Second)
			}
			if s.LastAttempt.After(s.LastSuccess) && q.ResetAt.After(s.LastAttempt) && (q.UsedPercent > 0 || stable) {
				s.LastSuccess = now
				s.VerifiedReset = q.ResetAt
				s.Successes++
				s.Status = "window_started"
			}
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		if s.IdleSince.IsZero() {
			s.IdleSince = now
		}
		if s.NextAttempt.After(now) {
			s.Status = "awaiting_window_confirmation"
			s.NextCheck = minTime(s.NextAttempt, now.Add(5*time.Minute))
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		readyAt := s.IdleSince.Add(time.Duration(e.cfg.GraceSeconds) * time.Second)
		if !q.ResetAt.IsZero() && !moving && q.ResetAt.Before(now) {
			readyAt = maxTime(readyAt, q.ResetAt.Add(time.Duration(e.cfg.GraceSeconds)*time.Second))
		}
		if now.Before(readyAt) {
			s.Status = "idle_grace"
			s.NextCheck = readyAt
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		if e.cfg.DryRun {
			s.Status = "would_start_window"
			s.NextCheck = now.Add(time.Duration(e.cfg.PollSeconds) * time.Second)
			if !e.commit(a.Index, s) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			return
		}
		// A durable reservation is made BEFORE touching the generation endpoint.
		// Even ambiguous failures/process crashes cannot cause retry storms.
		s.LastAttempt = now
		s.NextAttempt = now.Add(window + time.Duration(e.cfg.GraceSeconds)*time.Second)
		s.Attempts++
		s.Status = "request_reserved"
		s.NextCheck = now.Add(time.Duration(e.cfg.PollSeconds) * time.Second)
		if !e.commit(a.Index, s) {
			return
		}
		err = e.backend.Wake(ctx, a, e.cfg)
		if err != nil {
			s.Status = "request_failed_waiting_verification"
			s.LastError = err.Error()
		} else {
			s.Status = "request_sent_waiting_verification"
		}
		if !e.commit(a.Index, s) {
			return
		}
	}
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (e *Engine) Close() error {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()
	if e.store == nil {
		return errors.New("engine already closed")
	}
	err := e.store.Close()
	e.store = nil
	return err
}
