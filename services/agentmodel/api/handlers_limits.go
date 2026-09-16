package api

import (
	"net/http"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/router"
)

// GET /v1/limits — quota/rate-limit snapshot.
//
// This is the read side of the rate-limit control plane: callers (agent-shell's
// `limits` probe and other HTTP clients) poll it to learn each deployment's
// configured caps and whether any deployment is currently parked in a cooldown,
// so they can make routing/backoff decisions without burning a request.
//
// The response shape is the cross-service contract consumed by
// services/agentshell/limits.go (BackendLimits / LimitWindow). It is mirrored
// here rather than imported to keep the dependency direction one-way
// (agentshell depends on agentmodel, not the reverse). Keep the JSON tags in
// sync with that consumer.
//
// One window is emitted per configured cap (rpm → unit "requests", tpm → unit
// "tokens") carrying live current-minute consumption from the router's rate
// meter: used/remaining/used_percent, with window_status "limited" + a reset
// at the next minute boundary once the cap is reached — the same counters
// pre-call enforcement skips deployments on. A deployment currently in
// a failure cooldown is likewise "limited" with reset_at/reset_in_seconds; if
// it has no configured caps, a synthetic "cooldown" window carries that
// signal so the state is visible regardless.
//
// Configured spend caps (config `budget` and `keys[].max_budget`) are reported
// the same way, one window per cap with unit "usd" (org/<id>:budget,
// key/<name>:budget). Budgets ARE enforced on the spending endpoints
// (400 budget_exceeded), but this endpoint does not yet meter live
// spend, so used/remaining stay null and budget windows always report
// status "ok" — the rejection itself is the live exhaustion signal for now.

const (
	limitsWindow      = "1m"
	limitStatusOK     = "ok"
	limitStatusLimitd = "limited"
	limitSource       = "agentmodel"
	limitUnitRequests = "requests"
	limitUnitTokens   = "tokens"
	limitUnitStatus   = "status"
	limitUnitUSD      = "usd"
	limitReasonNoRtr  = "agentmodel_router_not_configured"
	cooldownRawMsg    = "deployment in rate-limit cooldown"
)

type limitWindow struct {
	LimitName        string     `json:"limit_name"`
	Window           string     `json:"window,omitempty"`
	Unit             string     `json:"unit"`
	Limit            *float64   `json:"limit,omitempty"`
	Remaining        *float64   `json:"remaining,omitempty"`
	Used             *float64   `json:"used,omitempty"`
	UsedPercent      *float64   `json:"used_percent,omitempty"`
	ResetAt          *time.Time `json:"reset_at,omitempty"`
	ResetInSeconds   *int64     `json:"reset_in_seconds,omitempty"`
	ResetDescription string     `json:"reset_description,omitempty"`
	Source           string     `json:"source"`
	AsOf             time.Time  `json:"as_of"`
	WindowStatus     string     `json:"window_status"`
	RawMessage       string     `json:"raw_message,omitempty"`
}

type limitsResponse struct {
	Agent             string        `json:"agent"`
	Status            string        `json:"status"`
	Windows           []limitWindow `json:"windows"`
	UnsupportedReason string        `json:"unsupported_reason,omitempty"`
}

func (s *Server) limits(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	out := limitsResponse{
		Agent:   limitSource,
		Status:  limitStatusOK,
		Windows: []limitWindow{},
	}

	// Spend caps don't depend on the router, so they are reported even when
	// rate-limit windows are unavailable — meaning the router-nil path below
	// can return budget windows alongside unsupported_reason. That pairing is
	// intentional: the reason describes the missing rate-limit side only.
	out.Windows = append(out.Windows, s.budgetWindows(now)...)

	if s.router == nil {
		out.UnsupportedReason = limitReasonNoRtr
		writeJSON(w, http.StatusOK, out)
		return
	}

	for _, dl := range s.router.DeploymentLimits(now) {
		cooled := dl.CooledUntil != nil
		windowStatus := limitStatusOK
		if cooled {
			windowStatus = limitStatusLimitd
		}

		emitted := 0
		if dl.RPM != nil {
			out.Windows = append(out.Windows, capWindow(dl, "rpm", limitUnitRequests, *dl.RPM, dl.UsedRequests, windowStatus, now))
			emitted++
		}
		if dl.TPM != nil {
			out.Windows = append(out.Windows, capWindow(dl, "tpm", limitUnitTokens, *dl.TPM, dl.UsedTokens, windowStatus, now))
			emitted++
		}
		// Surface a cooldown even when the deployment has no configured caps,
		// so a currently-limited deployment is never invisible.
		if cooled && emitted == 0 {
			win := limitWindow{
				LimitName:    dl.ModelName + "/" + dl.Deployment + ":cooldown",
				Unit:         limitUnitStatus,
				Source:       limitSource,
				AsOf:         now,
				WindowStatus: limitStatusLimitd,
				RawMessage:   cooldownRawMsg,
			}
			applyCooldown(&win, *dl.CooledUntil, now)
			out.Windows = append(out.Windows, win)
		}
	}

	// Top-level status is a pure function of the built windows — no flag to
	// keep in sync as window kinds grow.
	out.Status = rollupWindowStatus(out.Windows)
	writeJSON(w, http.StatusOK, out)
}

// budgetWindows reports the configured spend caps: the org-wide `budget` and
// each key's `max_budget`. Key names are config identifiers, never tokens, so
// they are safe to expose. Disabled (fail-closed) keys still report their
// resolved cap.
func (s *Server) budgetWindows(now time.Time) []limitWindow {
	var wins []limitWindow
	if s.orgCap != nil {
		wins = append(wins, budgetWindow("org/"+defaultOrgID+":budget", s.orgCap, now))
	}
	for i := range s.keys {
		if s.keys[i].cap == nil {
			continue
		}
		wins = append(wins, budgetWindow("key/"+s.keys[i].name+":budget", s.keys[i].cap, now))
	}
	return wins
}

// budgetWindow builds a window for one spend cap. Used/remaining are left nil
// and the status is always "ok" until live metering lands — budgets
// are enforced at request time, just not yet observable here.
func budgetWindow(name string, cap *spendCap, now time.Time) limitWindow {
	limit := cap.maxUSD
	desc := "lifetime cap; never resets"
	if cap.windowStr != "" {
		// Matches the documented reset semantics: fixed UTC windows computed
		// with time.Truncate(budget_duration).
		desc = "fixed UTC window; resets every " + cap.windowStr
	}
	return limitWindow{
		LimitName:        name,
		Window:           cap.windowStr, // "" (lifetime) is omitted by omitempty
		Unit:             limitUnitUSD,
		Limit:            &limit,
		ResetDescription: desc,
		Source:           limitSource,
		AsOf:             now,
		WindowStatus:     limitStatusOK,
	}
}

// fillUsage adds the live current-minute consumption to an rpm/tpm window
// metering: used, remaining, used_percent, and — when the cap is
// reached — window_status "limited" with a reset at the next minute boundary,
// matching what pre-call enforcement will do to requests until then.
//
// used_percent is clamped to 100 even though `used` can legitimately exceed
// the cap (tpm blocks only once observed usage reaches it, so one response
// routinely overshoots): every other producer of this window contract clamps
// (services/agentshell/limits.go clampPercent), and consumers assume the
// invariant. `used` itself stays unclamped — it is a true count.
func fillUsage(win *limitWindow, used, capValue int, now time.Time) {
	if capValue <= 0 {
		// Unreachable via LoadConfig (caps validate > 0); guards direct
		// construction from producing NaN/Inf that would blank the response.
		return
	}
	u := float64(used)
	rem := float64(capValue - used)
	if rem < 0 {
		rem = 0
	}
	pct := u / float64(capValue) * 100
	if pct > 100 {
		pct = 100
	}
	win.Used = &u
	win.Remaining = &rem
	win.UsedPercent = &pct
	if used < capValue {
		return
	}
	minuteReset := now.Truncate(time.Minute).Add(time.Minute)
	if win.WindowStatus == limitStatusLimitd {
		// Already limited by a failure cooldown. The deployment is usable
		// only when BOTH constraints clear, so advertise the later reset —
		// a sub-minute cooldown expiring before the minute boundary would
		// otherwise promise availability the cap still denies.
		if win.ResetAt == nil || win.ResetAt.Before(minuteReset) {
			applyCooldown(win, minuteReset, now)
		}
		return
	}
	win.WindowStatus = limitStatusLimitd
	applyCooldown(win, minuteReset, now)
	win.RawMessage = "deployment at its per-minute cap"
}

// capWindow builds a complete window for one configured cap (rpm or tpm):
// the cap, any active cooldown, and the live current-minute usage.
func capWindow(dl router.DeploymentLimit, kind, unit string, capValue, used int, status string, now time.Time) limitWindow {
	limit := float64(capValue)
	win := limitWindow{
		LimitName:    dl.ModelName + "/" + dl.Deployment + ":" + kind,
		Window:       limitsWindow,
		Unit:         unit,
		Limit:        &limit,
		Source:       limitSource,
		AsOf:         now,
		WindowStatus: status,
	}
	if dl.CooledUntil != nil {
		applyCooldown(&win, *dl.CooledUntil, now)
	}
	fillUsage(&win, used, capValue, now)
	return win
}

func applyCooldown(win *limitWindow, until, now time.Time) {
	u := until
	win.ResetAt = &u
	if secs := int64(until.Sub(now).Seconds()); secs > 0 {
		win.ResetInSeconds = &secs
	}
	if win.RawMessage == "" {
		win.RawMessage = cooldownRawMsg
	}
}
