package api

import (
	"strings"
	"testing"
	"time"
)

func TestAnthropicRateLimitedErrorString(t *testing.T) {
	if got := (&anthropicRateLimitedError{}).Error(); got != "claude oauth usage: rate limited (HTTP 429)" {
		t.Fatalf("rate limited error = %q", got)
	}
	if got := (&anthropicRateLimitedError{retryAfter: 42 * time.Second}).Error(); !strings.Contains(got, "retry after 42s") {
		t.Fatalf("rate limited error = %q, want retry-after annotation", got)
	}
}

func TestParseAnthropicTime(t *testing.T) {
	if _, err := parseAnthropicTime("not-a-time"); err == nil {
		t.Fatal("parseAnthropicTime(invalid) = nil err, want error")
	}
	got, err := parseAnthropicTime("2026-05-21T15:00:00.000Z")
	if err != nil {
		t.Fatalf("parseAnthropicTime: %v", err)
	}
	if !got.Equal(time.Date(2026, 5, 21, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("parseAnthropicTime = %v", got)
	}
}

func TestAnthropicUsageWindows(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	u35, u100 := 35.0, 100.0
	resp := anthropicUsageResponse{
		FiveHour:       &anthropicUsageWindow{Utilization: &u35, ResetsAt: "2026-05-21T15:00:00Z"},
		SevenDaySonnet: &anthropicUsageWindow{Utilization: &u100, ResetsAt: "2026-05-28T12:00:00Z"},
	}
	windows := anthropicUsageWindows(resp, now)
	if len(windows) != 2 {
		t.Fatalf("windows = %#v, want 2 (nil source windows skipped)", windows)
	}
	if windows[0].LimitName != "claude_5h" || windows[0].UsedPercent == nil || *windows[0].UsedPercent != 35 ||
		windows[0].Source != limitSourceClaudeOAuth || windows[0].WindowStatus != limitStatusOK {
		t.Fatalf("five_hour = %#v", windows[0])
	}
	if windows[1].LimitName != "claude_sonnet_weekly" || windows[1].WindowStatus != limitStatusLimitd {
		t.Fatalf("sonnet = %#v, want limited", windows[1])
	}
	if windows[0].Remaining == nil || *windows[0].Remaining != 65 {
		t.Fatalf("five_hour remaining = %v, want 65", windows[0].Remaining)
	}
}
