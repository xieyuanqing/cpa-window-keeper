package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

// Quota is the normalized quota state used by the window keeper.
type Quota struct {
	Supported    bool      `json:"supported"`
	Idle         bool      `json:"idle"`
	ResetAt      time.Time `json:"reset_at,omitempty"`
	UsedPercent  float64   `json:"used_percent"`
	BlockedUntil time.Time `json:"blocked_until,omitempty"`
	BlockReason  string    `json:"block_reason,omitempty"`
}

const codexFiveHourWindowSeconds int64 = 5 * 60 * 60
const codexWeeklyWindowSeconds int64 = 7 * 24 * 60 * 60

// ParseQuota parses one successful quota response without making any external
// calls. An incomplete or malformed response is an error rather than an idle
// account, because treating unknown state as idle could trigger a paid request.
func ParseQuota(provider string, body []byte, now time.Time) (Quota, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "codex":
		return parseCodexQuota(body, now)
	case "claude":
		return parseClaudeQuota(body, now)
	default:
		return Quota{}, fmt.Errorf("unsupported quota provider")
	}
}

type jsonObject map[string]json.RawMessage

func decodeJSONObject(data []byte) (jsonObject, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var object jsonObject
	if err := decoder.Decode(&object); err != nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("invalid JSON object: trailing data")
		}
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	if object == nil {
		return nil, fmt.Errorf("JSON value must be an object")
	}
	return object, nil
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func field(object jsonObject, name string) (json.RawMessage, bool) {
	raw, ok := object[name]
	return raw, ok
}

func rejectErrorField(object jsonObject) error {
	if raw, ok := field(object, "error"); ok && !isJSONNull(raw) {
		return fmt.Errorf("quota response contains an error field")
	}
	return nil
}

func requiredRaw(object jsonObject, name string) (json.RawMessage, error) {
	raw, ok := field(object, name)
	if !ok {
		return nil, fmt.Errorf("missing field %q", name)
	}
	if isJSONNull(raw) {
		return nil, fmt.Errorf("field %q must not be null", name)
	}
	return raw, nil
}

func parseObjectField(object jsonObject, name string, required bool, allowNull bool) (jsonObject, bool, error) {
	raw, ok := field(object, name)
	if !ok {
		if required {
			return nil, false, fmt.Errorf("missing object field %q", name)
		}
		return nil, false, nil
	}
	if isJSONNull(raw) {
		if allowNull {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("object field %q must not be null", name)
	}
	value, err := decodeJSONObject(raw)
	if err != nil {
		return nil, false, fmt.Errorf("field %q: %w", name, err)
	}
	return value, true, nil
}

func parseBoolField(object jsonObject, name string, required bool) (bool, error) {
	raw, ok := field(object, name)
	if !ok {
		if required {
			return false, fmt.Errorf("missing boolean field %q", name)
		}
		return false, nil
	}
	if isJSONNull(raw) {
		return false, fmt.Errorf("boolean field %q must not be null", name)
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, fmt.Errorf("field %q must be a boolean", name)
	}
	return value, nil
}

func parseFloatField(object jsonObject, name string, required bool) (float64, error) {
	raw, ok := field(object, name)
	if !ok {
		if required {
			return 0, fmt.Errorf("missing numeric field %q", name)
		}
		return 0, nil
	}
	if isJSONNull(raw) {
		return 0, fmt.Errorf("numeric field %q must not be null", name)
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("field %q must be a finite JSON number", name)
	}
	return value, nil
}

func parseInt64Raw(raw json.RawMessage, name string) (int64, error) {
	if isJSONNull(raw) {
		return 0, fmt.Errorf("field %q must be an integer", name)
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, fmt.Errorf("field %q must be an integer", name)
	}
	return value, nil
}

func parseInt64Field(object jsonObject, name string, required bool) (int64, bool, error) {
	raw, ok := field(object, name)
	if !ok {
		if required {
			return 0, false, fmt.Errorf("missing integer field %q", name)
		}
		return 0, false, nil
	}
	value, err := parseInt64Raw(raw, name)
	if err != nil {
		return 0, false, err
	}
	return value, true, nil
}

func parseStringRaw(raw json.RawMessage, name string) (string, error) {
	if isJSONNull(raw) {
		return "", fmt.Errorf("field %q must be a string", name)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("field %q must be a string", name)
	}
	return value, nil
}

func parseNullableStringField(object jsonObject, name string) (string, bool, error) {
	raw, ok := field(object, name)
	if !ok || isJSONNull(raw) {
		return "", false, nil
	}
	value, err := parseStringRaw(raw, name)
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func validatePercent(value float64, name string) error {
	if value < 0 || value > 100 {
		return fmt.Errorf("field %q must be between 0 and 100", name)
	}
	return nil
}

func parseCodexEpoch(raw json.RawMessage, name string) (time.Time, error) {
	seconds, err := parseInt64Raw(raw, name)
	if err != nil {
		return time.Time{}, err
	}
	if seconds < 0 {
		return time.Time{}, fmt.Errorf("field %q must not be negative", name)
	}
	return time.Unix(seconds, 0).UTC(), nil
}

type codexWindow struct {
	name          string
	usedPercent   float64
	limitSeconds  int64
	resetAt       time.Time
	hasResetAt    bool
	resetAfter    int64
	hasResetAfter bool
}

func parseCodexWindow(rateLimit jsonObject, name string) (*codexWindow, error) {
	raw, ok := field(rateLimit, name)
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	window, err := decodeJSONObject(raw)
	if err != nil {
		return nil, fmt.Errorf("field %q: %w", name, err)
	}

	usedPercent, err := parseFloatField(window, "used_percent", true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if err := validatePercent(usedPercent, name+".used_percent"); err != nil {
		return nil, err
	}
	limitSeconds, _, err := parseInt64Field(window, "limit_window_seconds", true)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if limitSeconds <= 0 {
		return nil, fmt.Errorf("%s.limit_window_seconds must be positive", name)
	}

	result := &codexWindow{
		name:         name,
		usedPercent:  usedPercent,
		limitSeconds: limitSeconds,
	}
	if rawReset, ok := field(window, "reset_at"); ok && !isJSONNull(rawReset) {
		result.resetAt, err = parseCodexEpoch(rawReset, name+".reset_at")
		if err != nil {
			return nil, err
		}
		result.hasResetAt = true
	}

	if resetAfter, ok, err := parseInt64Field(window, "reset_after_seconds", false); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	} else if ok {
		if resetAfter < 0 {
			return nil, fmt.Errorf("%s.reset_after_seconds must not be negative", name)
		}
		result.resetAfter = resetAfter
		result.hasResetAfter = true
	}
	return result, nil
}

func codexWindowIsFiveHour(window *codexWindow) bool {
	return window != nil && window.limitSeconds == codexFiveHourWindowSeconds
}

func codexWindowIsWeekly(window *codexWindow) bool {
	return window != nil && window.limitSeconds == codexWeeklyWindowSeconds
}

func addBlock(q *Quota, reason string, until time.Time) {
	if q.BlockReason == "" {
		q.BlockReason = reason
	} else if reason != "" {
		q.BlockReason += "; " + reason
	}
	if !until.IsZero() && until.After(q.BlockedUntil) {
		q.BlockedUntil = until
	}
	q.Idle = false
}

func parseCodexQuota(body []byte, now time.Time) (Quota, error) {
	root, err := decodeJSONObject(body)
	if err != nil {
		return Quota{}, err
	}
	if err := rejectErrorField(root); err != nil {
		return Quota{}, err
	}

	rateLimit, _, err := parseObjectField(root, "rate_limit", true, false)
	if err != nil {
		return Quota{}, err
	}
	allowed, err := parseBoolField(rateLimit, "allowed", true)
	if err != nil {
		return Quota{}, err
	}
	limitReached, err := parseBoolField(rateLimit, "limit_reached", true)
	if err != nil {
		return Quota{}, err
	}

	windows := make([]*codexWindow, 0, 2)
	for _, name := range []string{"primary_window", "secondary_window"} {
		window, err := parseCodexWindow(rateLimit, name)
		if err != nil {
			return Quota{}, err
		}
		if window != nil {
			windows = append(windows, window)
		}
	}
	if len(windows) == 0 {
		return Quota{}, fmt.Errorf("codex rate_limit has no quota windows")
	}
	for _, window := range windows {
		if window.hasResetAt {
			continue
		}
		// An exhausted weekly window without a reset is represented as a
		// hard block below. Every other window must carry a reset timestamp;
		// silently accepting a missing timestamp would make incomplete data
		// look usable.
		if codexWindowIsWeekly(window) && window.usedPercent >= 100 {
			continue
		}
		return Quota{}, fmt.Errorf("%s.reset_at is required", window.name)
	}

	var fiveHourWindows []*codexWindow
	for _, window := range windows {
		if codexWindowIsFiveHour(window) {
			if !window.hasResetAt {
				return Quota{}, fmt.Errorf("%s.reset_at is required for a five-hour window", window.name)
			}
			fiveHourWindows = append(fiveHourWindows, window)
		}
	}
	if len(fiveHourWindows) == 0 {
		// A valid Codex response for a free/monthly plan is intentionally not
		// treated as a five-hour-supported account.
		return Quota{Supported: false}, nil
	}

	quota := Quota{Supported: true}
	var activeFiveHour *codexWindow
	var chosenFiveHour *codexWindow
	for _, window := range fiveHourWindows {
		if window.hasResetAt && window.resetAt.After(now) {
			if activeFiveHour == nil || window.name == "primary_window" {
				activeFiveHour = window
			}
		}
	}
	if activeFiveHour != nil {
		chosenFiveHour = activeFiveHour
		quota.Idle = false
	} else {
		// Prefer primary_window for expired snapshots, while still accepting a
		// five-hour secondary_window when the primary is a different window.
		chosenFiveHour = fiveHourWindows[0]
		for _, window := range fiveHourWindows {
			if window.name == "primary_window" {
				chosenFiveHour = window
				break
			}
		}
		quota.Idle = true
	}
	quota.ResetAt = chosenFiveHour.resetAt
	quota.UsedPercent = chosenFiveHour.usedPercent

	var relevantExhausted, activeExhausted bool
	for _, window := range windows {
		if !codexWindowIsFiveHour(window) && !codexWindowIsWeekly(window) {
			continue
		}
		if window.usedPercent < 100 {
			continue
		}
		relevantExhausted = true
		if !window.hasResetAt {
			activeExhausted = true
			if codexWindowIsWeekly(window) {
				addBlock(&quota, "codex weekly quota exhausted without a reset time", time.Time{})
			}
			continue
		}
		if window.resetAt.After(now) {
			activeExhausted = true
			if codexWindowIsFiveHour(window) {
				addBlock(&quota, "codex five-hour quota exhausted", window.resetAt)
			} else {
				addBlock(&quota, "codex weekly quota exhausted", window.resetAt)
			}
		}
	}

	if !allowed && !limitReached {
		addBlock(&quota, "codex request rejected without a reported limit", time.Time{})
	}
	if limitReached && !activeExhausted && !relevantExhausted {
		// A stale limit_reached=true is tolerated only when every known
		// exhausted window is demonstrably expired. With no exhausted window,
		// the rejection cannot be explained safely.
		addBlock(&quota, "codex limit state could not be explained", time.Time{})
	}

	return quota, nil
}

type claudeWindow struct {
	name            string
	present         bool
	null            bool
	utilization     float64
	resetAt         time.Time
	hasResetAt      bool
	resetTimeAbsent bool
}

func parseClaudeReset(window jsonObject, name string, utilization float64, allowMissingResetOnOverage bool) (time.Time, bool, bool, error) {
	raw, ok := field(window, "resets_at")
	if !ok {
		if allowMissingResetOnOverage && utilization >= 100 {
			return time.Time{}, false, true, nil
		}
		return time.Time{}, false, false, fmt.Errorf("missing field %q", name+".resets_at")
	}
	if isJSONNull(raw) {
		if utilization == 0 {
			return time.Time{}, false, false, nil
		}
		if allowMissingResetOnOverage && utilization >= 100 {
			return time.Time{}, false, true, nil
		}
		return time.Time{}, false, false, fmt.Errorf("field %q may be null only when utilization is zero", name+".resets_at")
	}
	value, err := parseStringRaw(raw, name+".resets_at")
	if err != nil {
		return time.Time{}, false, false, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.IsZero() {
		return time.Time{}, false, false, fmt.Errorf("field %q must be a valid RFC3339 timestamp", name+".resets_at")
	}
	return parsed.UTC(), true, false, nil
}

func parseClaudeWindow(root jsonObject, name string, required bool, allowNull bool, allowMissingResetOnOverage bool) (claudeWindow, error) {
	raw, ok := field(root, name)
	if !ok {
		if required {
			return claudeWindow{}, fmt.Errorf("missing Claude quota field %q", name)
		}
		return claudeWindow{}, nil
	}
	if isJSONNull(raw) {
		if !allowNull {
			return claudeWindow{}, fmt.Errorf("Claude quota field %q must not be null", name)
		}
		return claudeWindow{name: name, present: true, null: true}, nil
	}
	object, err := decodeJSONObject(raw)
	if err != nil {
		return claudeWindow{}, fmt.Errorf("field %q: %w", name, err)
	}
	utilization, err := parseFloatField(object, "utilization", true)
	if err != nil {
		return claudeWindow{}, fmt.Errorf("%s: %w", name, err)
	}
	if err := validatePercent(utilization, name+".utilization"); err != nil {
		return claudeWindow{}, err
	}
	resetAt, hasResetAt, resetTimeAbsent, err := parseClaudeReset(object, name, utilization, allowMissingResetOnOverage)
	if err != nil {
		return claudeWindow{}, err
	}
	return claudeWindow{
		name:            name,
		present:         true,
		utilization:     utilization,
		resetAt:         resetAt,
		hasResetAt:      hasResetAt,
		resetTimeAbsent: resetTimeAbsent,
	}, nil
}

func parseClaudeLockedReason(root jsonObject, name string) (string, bool, error) {
	raw, ok := field(root, name)
	if !ok || isJSONNull(raw) {
		return "", false, nil
	}
	value, err := parseStringRaw(raw, name)
	if err != nil {
		return "", false, err
	}
	return value, strings.TrimSpace(value) != "", nil
}

func addClaudeExhaustion(quota *Quota, window claudeWindow, now time.Time) {
	if !window.present || window.null || window.utilization < 100 {
		return
	}
	if window.resetTimeAbsent {
		addBlock(quota, "Claude quota exhausted without a reset time", time.Time{})
		return
	}
	if window.hasResetAt && window.resetAt.After(now) {
		addBlock(quota, "Claude quota exhausted", window.resetAt)
	}
}

func parseClaudeQuota(body []byte, now time.Time) (Quota, error) {
	root, err := decodeJSONObject(body)
	if err != nil {
		return Quota{}, err
	}
	if err := rejectErrorField(root); err != nil {
		return Quota{}, err
	}

	fiveHour, err := parseClaudeWindow(root, "five_hour", true, true, false)
	if err != nil {
		return Quota{}, err
	}
	sevenDay, err := parseClaudeWindow(root, "seven_day", true, false, true)
	if err != nil {
		return Quota{}, err
	}
	oauthApps, err := parseClaudeWindow(root, "seven_day_oauth_apps", false, true, true)
	if err != nil {
		return Quota{}, err
	}
	// These windows are validated when present, but they are model-specific
	// and must not block a Haiku/small-request probe.
	sonnet, err := parseClaudeWindow(root, "seven_day_sonnet", false, true, false)
	if err != nil {
		return Quota{}, err
	}
	opus, err := parseClaudeWindow(root, "seven_day_opus", false, true, false)
	if err != nil {
		return Quota{}, err
	}

	quota := Quota{Supported: true}
	if fiveHour.null {
		quota.Idle = true
	} else {
		quota.UsedPercent = fiveHour.utilization
		if fiveHour.hasResetAt {
			quota.ResetAt = fiveHour.resetAt
			quota.Idle = !fiveHour.resetAt.After(now)
		} else {
			// The only accepted object without a reset is utilization=0 with an
			// explicit null reset, which represents an unstarted window.
			quota.Idle = true
		}
	}

	if locked, nonEmpty, err := parseClaudeLockedReason(root, "locked_reason"); err != nil {
		return Quota{}, err
	} else if nonEmpty && locked != "" {
		addBlock(&quota, "Claude quota is locked", time.Time{})
	}
	if locked, nonEmpty, err := parseClaudeLockedReason(root, "five_hour_locked_reason"); err != nil {
		return Quota{}, err
	} else if nonEmpty && locked != "" {
		addBlock(&quota, "Claude quota is locked", time.Time{})
	}

	// locked_reason is part of the five_hour object in the live OAuth shape.
	if raw, ok := field(root, "five_hour"); ok && !isJSONNull(raw) {
		object, err := decodeJSONObject(raw)
		if err != nil {
			return Quota{}, fmt.Errorf("field %q: %w", "five_hour", err)
		}
		if locked, nonEmpty, err := parseClaudeLockedReason(object, "locked_reason"); err != nil {
			return Quota{}, err
		} else if nonEmpty && locked != "" {
			addBlock(&quota, "Claude quota is locked", time.Time{})
		}
	}

	addClaudeExhaustion(&quota, fiveHour, now)
	addClaudeExhaustion(&quota, sevenDay, now)
	addClaudeExhaustion(&quota, oauthApps, now)
	// Intentionally do not call addClaudeExhaustion for sonnet or opus.
	_ = sonnet
	_ = opus

	return quota, nil
}
