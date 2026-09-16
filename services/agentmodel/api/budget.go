package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
)

// Pre-request enforcement for every spending endpoint
// (/v1/chat/completions, /v1/messages, /v1/embeddings, /v1/images/generations,
// /v1/videos/generations, and the /v1/predictions* Replicate passthrough — each
// calls s.enforce()). Read endpoints (/v1/models, /v1/limits, /v1/usage, the
// GET job polls, oauth helpers) are never budget-gated: headroom must stay
// observable even when spend is exhausted.

// spendCap is one resolved, enforceable budget: a USD amount per window,
// with the original config string kept for display. nil *spendCap = no cap.
type spendCap struct {
	maxUSD    float64
	window    time.Duration // 0 = lifetime (whole ledger)
	windowStr string        // original config string; "" = lifetime
}

// resolveCap parses one configured (max_budget, budget_duration) pair into an
// enforceable cap, once, at construction — never on the request path.
// ok=false means a cap was configured but its window failed to parse:
// unreachable through LoadConfig (Validate rejects it), possible via direct
// api.Config construction; callers must fail closed.
func resolveCap(maxBudget *float64, duration string) (cap *spendCap, ok bool) {
	if maxBudget == nil {
		return nil, true
	}
	duration = strings.TrimSpace(duration)
	var window time.Duration
	if duration != "" {
		d, err := time.ParseDuration(duration)
		if err != nil || d <= 0 {
			return nil, false
		}
		window = d
	}
	return &spendCap{maxUSD: *maxBudget, window: window, windowStr: duration}, true
}

// windowStart returns the start of the cap's current budget window: fixed
// boundaries at multiples of the window since Go's zero time (the documented
// time.Truncate contract), or the zero time itself for a lifetime cap.
func windowStart(now time.Time, window time.Duration) time.Time {
	if window <= 0 {
		return time.Time{}
	}
	return now.UTC().Truncate(window)
}

func windowSuffix(windowStr string) string {
	if windowStr == "" {
		return " (lifetime)"
	}
	return " (window " + windowStr + ")"
}

// enforce runs the full pre-request policy sequence for one spending
// request: model allowlist first (a blocked entry model must be rejected
// 403 before any router walk — see the CompleteBlocked invariant), then
// budget caps. Handlers keep their own envelope/audit handling around the
// single returned error.
func (s *Server) enforce(ctx context.Context, vk *resolvedKey, model string) *agentmodel.Error {
	if ae := checkAccess(vk, model); ae != nil {
		return ae
	}
	return s.checkBudgets(ctx, vk)
}

// checkAccess enforces the authenticated key's model allowlist. nil error
// means allowed (master token, unrestricted key, or model in allowlist).
func checkAccess(vk *resolvedKey, model string) *agentmodel.Error {
	if vk.allowsModel(model) {
		return nil
	}
	return agentmodel.NewErrorCode(agentmodel.ErrTypePermissionDenied, agentmodel.CodeModelAccessDenied,
		fmt.Sprintf("key %q is not allowed to use model %q", vk.name, model))
}

// checkBudgets rejects the request before any upstream call when the org cap
// or the authenticated key's cap is already reached (spend >= max_budget,
// LiteLLM semantics: the cap is a hard limit on accumulated spend, not a
// prediction of the next request's cost). Spend is read live from the cost
// ledger over the current budget window.
//
// Known overshoot windows, accepted by design (same trade-off as LiteLLM's
// pre-call check): concurrent in-flight requests each pass the check before
// any of them lands its cost row, and a streaming response's cost only enters
// the ledger after the stream completes — so spend can exceed the cap by
// roughly (in-flight requests x per-request cost) before rejections start.
// The cap bounds sustained spend, not instantaneous spend. Note also that the
// ledger meters registry-priced spend: a model missing from the price catalog
// records $0 (cost_source="unpriced") and never advances any cap — watch the
// agentmodel_cost_source_total{source="unpriced"} metric.
//
// Fail-closed: if a cap is configured but the ledger is unavailable (or the
// org cap was constructed with an unparseable window), the request is
// rejected — a budget that silently stops being enforced is not a budget.
// The client sees a generic 503; the cause is logged server-side only.
func (s *Server) checkBudgets(ctx context.Context, vk *resolvedKey) *agentmodel.Error {
	if s.orgCapInvalid {
		return s.budgetUnavailable(ctx, "org budget", errors.New("budget_duration failed to parse at construction"))
	}
	keyCap := vk != nil && vk.cap != nil
	if s.orgCap == nil && !keyCap {
		return nil
	}
	if s.store == nil {
		return s.budgetUnavailable(ctx, "spend ledger", errors.New("no store configured"))
	}

	now := time.Now()
	if s.orgCap != nil {
		// Precise org-wide spend is operator information: a tenant key only
		// learns that the shared cap is exhausted, not how much the org spent.
		ae := s.checkCap(ctx, now, fmt.Sprintf("org %q", defaultOrgID), s.orgCap, vk != nil,
			func(since time.Time) (float64, error) { return s.store.SumCostByOrg(ctx, defaultOrgID, since) })
		if ae != nil {
			return ae
		}
	}
	if keyCap {
		ae := s.checkCap(ctx, now, fmt.Sprintf("key %q", vk.name), vk.cap, false,
			func(since time.Time) (float64, error) { return s.store.SumCostByAPIKey(ctx, vk.hash, since) })
		if ae != nil {
			return ae
		}
	}
	return nil
}

// checkCap evaluates one spend cap against the ledger. label names the scope
// in the rejection message; redact omits the dollar figures.
func (s *Server) checkCap(ctx context.Context, now time.Time, label string, cap *spendCap, redact bool, sum func(time.Time) (float64, error)) *agentmodel.Error {
	spent, err := sum(windowStart(now, cap.window))
	if err != nil {
		return s.budgetUnavailable(ctx, label+" spend lookup", err)
	}
	if spent < cap.maxUSD {
		return nil
	}
	msg := label + " budget exceeded" + windowSuffix(cap.windowStr)
	if !redact {
		msg = fmt.Sprintf("%s budget exceeded: spent $%.6f of $%.2f cap%s",
			label, spent, cap.maxUSD, windowSuffix(cap.windowStr))
	}
	return agentmodel.NewErrorCode(agentmodel.ErrTypeBudgetExceeded, agentmodel.CodeBudgetExceeded, msg)
}

// budgetUnavailable is the fail-closed rejection when a configured cap cannot
// be evaluated. service_unavailable (503) rather than budget_exceeded: the
// caller's budget state is unknown, not exhausted. The underlying error is
// logged server-side only — raw store/internal error text never reaches the
// client.
func (s *Server) budgetUnavailable(ctx context.Context, reason string, err error) *agentmodel.Error {
	s.logger.ErrorContext(ctx, "agentmodel: budget check unavailable — failing closed",
		"reason", reason, "error", err)
	return agentmodel.NewErrorCode(agentmodel.ErrTypeServiceUnavailable, agentmodel.CodeBudgetUnavailable,
		"budget configured but currently unenforceable; request rejected (fail-closed)")
}

// blockedModels returns the logical models the key may NOT use — the set the
// router must exclude from fallback walks so a restricted key's request never
// falls back onto a model outside its allowlist.
func (s *Server) blockedModels(vk *resolvedKey) []string {
	if vk == nil || vk.models == nil || s.router == nil {
		return nil
	}
	var blocked []string
	for _, m := range s.router.Models() {
		if !vk.models[m.Name] {
			blocked = append(blocked, m.Name)
		}
	}
	return blocked
}
