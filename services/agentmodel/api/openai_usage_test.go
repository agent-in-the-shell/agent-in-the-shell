package api

import (
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// TestOpenAIUsageWindows covers the mapping from chatgpt.com/backend-api/codex's
// /usage payload onto limitWindow. The naming rule is ported from agentshell's
// codex probe: a window is named from its OWN duration, never from the slot it
// arrives in, because OpenAI moved the weekly window into the primary slot when
// it dropped the 5-hour limit on 2026-07-12. This endpoint states the duration
// explicitly (limit_window_seconds), so the rule is simply enforceable here.
func TestOpenAIUsageWindows(t *testing.T) {
	now := time.Unix(1786000000, 0).UTC()
	usage := openaiUsageResponse{
		PlanType: "pro",
		RateLimit: &openaiRateLimit{
			// 604800s = weekly, arriving in the PRIMARY slot post-2026-07-12.
			PrimaryWindow: &openaiWindow{UsedPercent: f64(12), LimitWindowSeconds: i64(604800), ResetAt: i64(1786766504)},
		},
		AdditionalRateLimits: []openaiAdditionalRateLimit{{
			LimitName:      "GPT-5.3-Codex-Spark",
			MeteredFeature: "codex_bengalfox",
			RateLimit: &openaiRateLimit{
				PrimaryWindow: &openaiWindow{UsedPercent: f64(100), LimitWindowSeconds: i64(604800), ResetAt: i64(1786776142)},
			},
		}},
	}

	got := openaiUsageWindows(usage, now)
	if len(got) != 2 {
		t.Fatalf("got %d windows, want 2: %+v", len(got), got)
	}

	base := got[0]
	if base.LimitName != "codex_weekly" {
		t.Errorf("base LimitName = %q, want codex_weekly (named from its own 604800s duration, not its slot)", base.LimitName)
	}
	if base.Window != "7d" {
		t.Errorf("base Window = %q, want 7d", base.Window)
	}
	if base.UsedPercent == nil || *base.UsedPercent != 12 {
		t.Errorf("base UsedPercent = %v, want 12", base.UsedPercent)
	}
	if base.WindowStatus != limitStatusOK {
		t.Errorf("base WindowStatus = %q, want ok", base.WindowStatus)
	}
	if base.RawMessage != "" {
		t.Errorf("base RawMessage = %q, want empty (no display label on the base limit)", base.RawMessage)
	}

	perModel := got[1]
	if perModel.LimitName != "codex_bengalfox_weekly" {
		t.Errorf("per-model LimitName = %q, want codex_bengalfox_weekly (identity from metered_feature)", perModel.LimitName)
	}
	if perModel.RawMessage != "GPT-5.3-Codex-Spark" {
		t.Errorf("per-model RawMessage = %q, want the display label from limit_name", perModel.RawMessage)
	}
	if perModel.WindowStatus != limitStatusLimitd {
		t.Errorf("per-model WindowStatus = %q, want limited at 100%%", perModel.WindowStatus)
	}
}

// A duration OpenAI has not shipped is named for what it is rather than
// borrowing a neighbouring label, so two windows can never share a name.
func TestOpenAIUsageWindowDurationNaming(t *testing.T) {
	for _, tc := range []struct {
		seconds          int64
		wantName, wantWi string
	}{
		{604800, "codex_weekly", "7d"}, // continuity: the report has always called this weekly
		{18000, "codex_5h", "5h"},
		{2592000, "codex_30d", "30d"},
		{5400, "codex_90m", "90m"},
	} {
		usage := openaiUsageResponse{RateLimit: &openaiRateLimit{
			PrimaryWindow: &openaiWindow{UsedPercent: f64(0), LimitWindowSeconds: i64(tc.seconds)},
		}}
		got := openaiUsageWindows(usage, time.Unix(1786000000, 0).UTC())
		if len(got) != 1 {
			t.Fatalf("%ds: got %d windows, want 1", tc.seconds, len(got))
		}
		if got[0].LimitName != tc.wantName || got[0].Window != tc.wantWi {
			t.Errorf("%ds: got (%q,%q), want (%q,%q)", tc.seconds, got[0].LimitName, got[0].Window, tc.wantName, tc.wantWi)
		}
	}
}

// A window carrying no usable duration is dropped rather than given a name that
// would be wrong — two rows sharing a LimitName would print identical labels
// over contradicting numbers.
func TestOpenAIUsageWindowsDropsUnnameableAndDedupes(t *testing.T) {
	now := time.Unix(1786000000, 0).UTC()
	usage := openaiUsageResponse{
		RateLimit: &openaiRateLimit{
			PrimaryWindow:   &openaiWindow{UsedPercent: f64(5), LimitWindowSeconds: i64(604800)},
			SecondaryWindow: &openaiWindow{UsedPercent: f64(9)}, // no duration → dropped
		},
		AdditionalRateLimits: []openaiAdditionalRateLimit{{
			MeteredFeature: "codex", // restates the base limit under its own id
			RateLimit: &openaiRateLimit{
				PrimaryWindow: &openaiWindow{UsedPercent: f64(5), LimitWindowSeconds: i64(604800)},
			},
		}},
	}
	got := openaiUsageWindows(usage, now)
	if len(got) != 1 {
		t.Fatalf("got %d windows, want 1 (unnameable dropped, restated base de-duplicated): %+v", len(got), got)
	}
	if got[0].LimitName != "codex_weekly" {
		t.Errorf("LimitName = %q, want codex_weekly", got[0].LimitName)
	}
}
