package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func codexWindowJSON(used float64, limit int64, resetAt string, includeReset bool) string {
	reset := ""
	if includeReset {
		reset = fmt.Sprintf(",\"reset_at\":%s", resetAt)
	}
	return fmt.Sprintf(`{"used_percent":%.1f,"limit_window_seconds":%d,"reset_after_seconds":60%s}`, used, limit, reset)
}

func codexBody(allowed, limitReached bool, primary, secondary string) string {
	return fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"allowed":%t,"limit_reached":%t,"primary_window":%s,"secondary_window":%s}}`, allowed, limitReached, primary, secondary)
}

func claudeWindowJSON(utilization float64, resetAt string, lockedReason string) string {
	locked := "null"
	if lockedReason != "" {
		locked = fmt.Sprintf("%q", lockedReason)
	}
	return fmt.Sprintf(`{"utilization":%.1f,"resets_at":%s,"locked_reason":%s}`, utilization, resetAt, locked)
}

func claudeBody(fiveHour, sevenDay, oauthApps, sonnet, opus string) string {
	return fmt.Sprintf(`{"five_hour":%s,"seven_day":%s,"seven_day_oauth_apps":%s,"seven_day_sonnet":%s,"seven_day_opus":%s}`, fiveHour, sevenDay, oauthApps, sonnet, opus)
}

type quotaExpectation struct {
	supported    bool
	idle         bool
	resetAt      time.Time
	usedPercent  float64
	blockedUntil time.Time
	blocked      bool
}

func assertQuota(t *testing.T, got Quota, want quotaExpectation) {
	t.Helper()
	if got.Supported != want.supported {
		t.Fatalf("Supported = %v, want %v; got %+v", got.Supported, want.supported, got)
	}
	if got.Idle != want.idle {
		t.Fatalf("Idle = %v, want %v; got %+v", got.Idle, want.idle, got)
	}
	if !got.ResetAt.Equal(want.resetAt) {
		t.Fatalf("ResetAt = %v, want %v; got %+v", got.ResetAt, want.resetAt, got)
	}
	if got.UsedPercent != want.usedPercent {
		t.Fatalf("UsedPercent = %v, want %v; got %+v", got.UsedPercent, want.usedPercent, got)
	}
	if !got.BlockedUntil.Equal(want.blockedUntil) {
		t.Fatalf("BlockedUntil = %v, want %v; want %v; got %+v", got.BlockedUntil, want.blockedUntil, want.blockedUntil, got)
	}
	if (got.BlockReason != "") != want.blocked {
		t.Fatalf("BlockReason presence = %v, want %v; reason=%q; got %+v", got.BlockReason != "", want.blocked, got.BlockReason, got)
	}
}

func TestParseQuotaCodex(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	activeReset := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
	weeklyReset := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	expiredReset := time.Date(2026, 9, 19, 11, 59, 59, 0, time.UTC)

	tests := []struct {
		name string
		body string
		want quotaExpectation
	}{
		{
			name: "active primary five hour window",
			body: codexBody(true, false,
				codexWindowJSON(38, codexFiveHourWindowSeconds, fmt.Sprintf("%d", activeReset.Unix()), true),
				codexWindowJSON(78, codexWeeklyWindowSeconds, fmt.Sprintf("%d", weeklyReset.Unix()), true)),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 38},
		},
		{
			name: "expired five hour snapshot remains idle even at one hundred percent",
			body: codexBody(true, true,
				codexWindowJSON(100, codexFiveHourWindowSeconds, fmt.Sprintf("%d", expiredReset.Unix()), true),
				codexWindowJSON(100, codexWeeklyWindowSeconds, fmt.Sprintf("%d", expiredReset.Unix()), true)),
			want: quotaExpectation{supported: true, idle: true, resetAt: expiredReset, usedPercent: 100},
		},
		{
			name: "secondary window may be the five hour window",
			body: codexBody(true, false,
				codexWindowJSON(61, codexWeeklyWindowSeconds, fmt.Sprintf("%d", weeklyReset.Unix()), true),
				codexWindowJSON(100, codexFiveHourWindowSeconds, fmt.Sprintf("%d", activeReset.Unix()), true)),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 100, blockedUntil: activeReset, blocked: true},
		},
		{
			name: "free monthly window is not five hour supported",
			body: codexBody(true, false,
				codexWindowJSON(12, 30*24*60*60, fmt.Sprintf("%d", weeklyReset.Unix()), true),
				"null"),
			want: quotaExpectation{supported: false},
		},
		{
			name: "future weekly exhaustion blocks until reset",
			body: codexBody(true, false,
				codexWindowJSON(20, codexFiveHourWindowSeconds, fmt.Sprintf("%d", activeReset.Unix()), true),
				codexWindowJSON(100, codexWeeklyWindowSeconds, fmt.Sprintf("%d", weeklyReset.Unix()), true)),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 20, blockedUntil: weeklyReset, blocked: true},
		},
		{
			name: "weekly exhaustion without reset is a hard block",
			body: codexBody(true, false,
				codexWindowJSON(20, codexFiveHourWindowSeconds, fmt.Sprintf("%d", activeReset.Unix()), true),
				codexWindowJSON(100, codexWeeklyWindowSeconds, "null", false)),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 20, blocked: true},
		},
		{
			name: "unknown rejection is conservative",
			body: codexBody(false, false,
				codexWindowJSON(20, codexFiveHourWindowSeconds, fmt.Sprintf("%d", activeReset.Unix()), true),
				codexWindowJSON(20, codexWeeklyWindowSeconds, fmt.Sprintf("%d", weeklyReset.Unix()), true)),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 20, blocked: true},
		},
		{
			name: "limit reached is tolerated when every exhausted window expired",
			body: codexBody(true, true,
				codexWindowJSON(100, codexFiveHourWindowSeconds, fmt.Sprintf("%d", expiredReset.Unix()), true),
				codexWindowJSON(100, codexWeeklyWindowSeconds, fmt.Sprintf("%d", expiredReset.Unix()), true)),
			want: quotaExpectation{supported: true, idle: true, resetAt: expiredReset, usedPercent: 100},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseQuota("codex", []byte(tt.body), now)
			if err != nil {
				t.Fatalf("ParseQuota returned error: %v", err)
			}
			assertQuota(t, got, tt.want)
		})
	}
}

func TestParseQuotaCodexRejectsIncompleteOrInvalidResponses(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	future := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
	validPrimary := codexWindowJSON(20, codexFiveHourWindowSeconds, fmt.Sprintf("%d", future.Unix()), true)

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: "{"},
		{name: "trailing JSON", body: codexBody(true, false, validPrimary, "null") + " {}"},
		{name: "error response", body: `{"error":{"type":"rate_limit_error"}}`},
		{name: "missing rate limit", body: `{}`},
		{name: "missing allowed", body: fmt.Sprintf(`{"rate_limit":{"limit_reached":false,"primary_window":%s}}`, validPrimary)},
		{name: "missing limit reached", body: fmt.Sprintf(`{"rate_limit":{"allowed":true,"primary_window":%s}}`, validPrimary)},
		{name: "missing both windows", body: `{"rate_limit":{"allowed":true,"limit_reached":false}}`},
		{name: "five hour used percent missing", body: fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"limit_window_seconds":18000,"reset_at":%d}}}`, future.Unix())},
		{name: "five hour reset missing", body: fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000}}}`)},
		{name: "negative used percent", body: codexBody(true, false, codexWindowJSON(-1, codexFiveHourWindowSeconds, fmt.Sprintf("%d", future.Unix()), true), "null")},
		{name: "used percent above one hundred", body: codexBody(true, false, codexWindowJSON(100.1, codexFiveHourWindowSeconds, fmt.Sprintf("%d", future.Unix()), true), "null")},
		{name: "negative window length", body: codexBody(true, false, codexWindowJSON(0, -1, fmt.Sprintf("%d", future.Unix()), true), "null")},
		{name: "invalid reset type", body: codexBody(true, false, codexWindowJSON(0, codexFiveHourWindowSeconds, `"not-a-timestamp"`, true), "null")},
		{name: "negative reset after", body: fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":-1,"reset_at":%d}}}`, future.Unix())},
		{name: "non numeric strict type", body: fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":"0","limit_window_seconds":18000,"reset_at":%d}}}`, future.Unix())},
		{name: "weekly underage reset missing", body: fmt.Sprintf(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":%s,"secondary_window":{"used_percent":20,"limit_window_seconds":604800}}}`, validPrimary)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseQuota("codex", []byte(tt.body), now)
			if err == nil {
				t.Fatalf("ParseQuota returned nil error and quota %+v", got)
			}
			if got.Idle {
				t.Fatalf("invalid response was reported idle: %+v", got)
			}
		})
	}

}

func TestParseQuotaClaude(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	activeReset := time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC)
	weeklyReset := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	expiredReset := time.Date(2026, 9, 19, 11, 59, 59, 0, time.UTC)
	activeResetJSON := fmt.Sprintf("%q", activeReset.Format(time.RFC3339Nano))
	weeklyResetJSON := fmt.Sprintf("%q", weeklyReset.Format(time.RFC3339Nano))
	expiredResetJSON := fmt.Sprintf("%q", expiredReset.Format(time.RFC3339Nano))
	validWeekly := claudeWindowJSON(85, weeklyResetJSON, "")

	tests := []struct {
		name string
		body string
		want quotaExpectation
	}{
		{
			name: "active five hour window",
			body: claudeBody(claudeWindowJSON(18, activeResetJSON, ""), validWeekly, "null", "null", "null"),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 18},
		},
		{
			name: "null five hour window is idle",
			body: claudeBody("null", validWeekly, "null", "null", "null"),
			want: quotaExpectation{supported: true, idle: true},
		},
		{
			name: "zero utilization with explicit null reset is idle",
			body: claudeBody(claudeWindowJSON(0, "null", ""), validWeekly, "null", "null", "null"),
			want: quotaExpectation{supported: true, idle: true, usedPercent: 0},
		},
		{
			name: "expired five hour snapshot is idle even at one hundred percent",
			body: claudeBody(claudeWindowJSON(100, expiredResetJSON, ""), claudeWindowJSON(100, expiredResetJSON, ""), "null", "null", "null"),
			want: quotaExpectation{supported: true, idle: true, resetAt: expiredReset, usedPercent: 100},
		},
		{
			name: "generic weekly quota blocks",
			body: claudeBody(claudeWindowJSON(18, activeResetJSON, ""), claudeWindowJSON(100, weeklyResetJSON, ""), "null", "null", "null"),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 18, blockedUntil: weeklyReset, blocked: true},
		},
		{
			name: "oauth apps weekly quota blocks",
			body: claudeBody(claudeWindowJSON(18, activeResetJSON, ""), validWeekly, claudeWindowJSON(100, weeklyResetJSON, ""), "null", "null"),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 18, blockedUntil: weeklyReset, blocked: true},
		},
		{
			name: "sonnet and opus specialized quotas do not block",
			body: claudeBody("null", validWeekly, "null", claudeWindowJSON(100, weeklyResetJSON, ""), claudeWindowJSON(100, weeklyResetJSON, "")),
			want: quotaExpectation{supported: true, idle: true},
		},
		{
			name: "five hour lock reason blocks",
			body: claudeBody(claudeWindowJSON(18, activeResetJSON, "member_zero_credit_limit"), validWeekly, "null", "null", "null"),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 18, blocked: true},
		},
		{
			name: "five hour exhaustion blocks until reset",
			body: claudeBody(claudeWindowJSON(100, activeResetJSON, ""), validWeekly, "null", "null", "null"),
			want: quotaExpectation{supported: true, idle: false, resetAt: activeReset, usedPercent: 100, blockedUntil: activeReset, blocked: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseQuota("claude", []byte(tt.body), now)
			if err != nil {
				t.Fatalf("ParseQuota returned error: %v", err)
			}
			assertQuota(t, got, tt.want)
		})
	}
}

func TestParseQuotaClaudeRejectsIncompleteOrInvalidResponses(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	futureJSON := fmt.Sprintf("%q", time.Date(2026, 9, 19, 13, 0, 0, 0, time.UTC).Format(time.RFC3339Nano))
	validFive := claudeWindowJSON(18, futureJSON, "")
	validSeven := claudeWindowJSON(85, futureJSON, "")

	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: "{"},
		{name: "trailing JSON", body: claudeBody(validFive, validSeven, "null", "null", "null") + " {}"},
		{name: "error response", body: `{"error":{"type":"invalid_token"}}`},
		{name: "missing five hour", body: fmt.Sprintf(`{"seven_day":%s}`, validSeven)},
		{name: "missing generic weekly field", body: fmt.Sprintf(`{"five_hour":%s}`, validFive)},
		{name: "five hour utilization missing", body: fmt.Sprintf(`{"five_hour":{"resets_at":%s},"seven_day":%s}`, futureJSON, validSeven)},
		{name: "five hour invalid time", body: fmt.Sprintf(`{"five_hour":{"utilization":18,"resets_at":"not-a-time"},"seven_day":%s}`, validSeven)},
		{name: "five hour negative utilization", body: fmt.Sprintf(`{"five_hour":{"utilization":-1,"resets_at":%s},"seven_day":%s}`, futureJSON, validSeven)},
		{name: "five hour utilization above one hundred", body: fmt.Sprintf(`{"five_hour":{"utilization":100.1,"resets_at":%s},"seven_day":%s}`, futureJSON, validSeven)},
		{name: "nonzero null five hour reset", body: fmt.Sprintf(`{"five_hour":{"utilization":18,"resets_at":null},"seven_day":%s}`, validSeven)},
		{name: "five hour reset wrong type", body: fmt.Sprintf(`{"five_hour":{"utilization":18,"resets_at":123},"seven_day":%s}`, validSeven)},
		{name: "locked reason wrong type", body: fmt.Sprintf(`{"five_hour":{"utilization":18,"resets_at":%s,"locked_reason":false},"seven_day":%s}`, futureJSON, validSeven)},
		{name: "weekly overage reset wrong type", body: fmt.Sprintf(`{"five_hour":%s,"seven_day":{"utilization":100,"resets_at":123}}`, validFive)},
		{name: "weekly nonzero reset missing is a hard parse error", body: fmt.Sprintf(`{"five_hour":%s,"seven_day":{"utilization":85}}`, validFive)},
		{name: "specialized malformed number", body: fmt.Sprintf(`{"five_hour":%s,"seven_day":%s,"seven_day_sonnet":{"utilization":101,"resets_at":%s}}`, validFive, validSeven, futureJSON)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseQuota("claude", []byte(tt.body), now)
			if err == nil {
				t.Fatalf("ParseQuota returned nil error and quota %+v", got)
			}
			if got.Idle {
				t.Fatalf("invalid response was reported idle: %+v", got)
			}
		})
	}
}

func TestParseQuotaRejectsUnknownProvider(t *testing.T) {
	got, err := ParseQuota("other", []byte(`{}`), time.Unix(1, 0))
	if err == nil {
		t.Fatalf("expected unknown provider error, got quota %+v", got)
	}
	if got != (Quota{}) {
		t.Fatalf("unknown provider returned non-zero quota: %+v", got)
	}
}

func TestParseQuotaDoesNotTreatErrorLikeIdle(t *testing.T) {
	for _, provider := range []string{"codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			got, err := ParseQuota(provider, []byte(`{"error":"upstream failure"}`), time.Now())
			if err == nil {
				t.Fatal("expected error response to fail")
			}
			if got.Idle || strings.TrimSpace(got.BlockReason) != "" {
				// An error is returned separately; it must never look like a
				// successful idle result.
				t.Fatalf("error response produced unsafe quota: %+v", got)
			}
		})
	}
}
