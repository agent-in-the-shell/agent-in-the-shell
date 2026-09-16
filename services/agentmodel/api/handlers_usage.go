package api

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/store"
)

// codeInvalidParameter and codeUsageReportFailed are the usage-endpoint error
// codes; they predate the shared code vocabulary and stay local to keep the
// wire output byte-identical (this only removes empty codes, never renames).
const (
	codeInvalidParameter  = "invalid_parameter"
	codeUsageReportFailed = "usage_report_failed"
)

// usageMaxLimit caps the row count a single /v1/usage call may return, and
// usageMaxWindow caps the lookback span — both cheap DoS guards on an
// unauthenticated-by-row aggregation.
const (
	usageMaxLimit     = 1000
	usageDefaultLimit = 100
	usageMaxWindow    = 366 * 24 * time.Hour
	usageDefaultSince = 7 * 24 * time.Hour
)

// usageResponse is the flat-rows envelope. A time bucket is just another `key`;
// the envelope keeps adding totals / next_cursor / nested buckets non-breaking.
type usageResponse struct {
	Object  string           `json:"object"`
	GroupBy string           `json:"group_by"`
	Window  usageWindow      `json:"window"`
	Rows    []store.UsageRow `json:"rows"`
}

type usageWindow struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// usage handles GET /v1/usage — a read-only rollup of request_logs for the
// authenticated org. The org is ALWAYS derived from the bearer context; a
// client-supplied ?org= is rejected (the Wave-1 multi-tenant-safe invariant).
func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	if q.Has("org") {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: codeInvalidParameter,
			Message: "org is derived from the API key and cannot be supplied as a query parameter"})
		return
	}

	gb := q.Get("group_by")
	if gb == "" {
		gb = string(store.DimModel)
	}
	dim, err := store.ParseDimension(gb)
	if err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: codeInvalidParameter, Message: err.Error()})
		return
	}

	now := time.Now().UTC()
	start, end, ok := parseUsageWindow(q, now)
	if !ok {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: codeInvalidParameter,
			Message: "invalid time window: start/end must be unix seconds with start < end, or use since=7d"})
		return
	}

	limit := usageDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, perr := strconv.Atoi(v)
		if perr != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: codeInvalidParameter, Message: "limit must be a positive integer"})
			return
		}
		limit = n
	}
	if limit > usageMaxLimit {
		limit = usageMaxLimit
	}

	rows, err := s.store.UsageReport(r.Context(), store.UsageFilter{
		OrgID: orgIDFromCtx(r.Context()),
		Start: start,
		End:   end,
		By:    []store.UsageDimension{dim},
		Limit: limit,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, &agentmodel.Error{Type: "internal_error", Code: codeUsageReportFailed, Message: err.Error()})
		return
	}
	if rows == nil {
		rows = []store.UsageRow{}
	}

	writeJSON(w, http.StatusOK, usageResponse{
		Object:  "usage.report",
		GroupBy: string(dim),
		Window:  usageWindow{Start: start.Unix(), End: end.Unix()},
		Rows:    rows,
	})
}

// parseUsageWindow resolves the [start, end) window from query params. Precedence:
// explicit start/end (unix seconds) win; otherwise `since` (e.g. 7d) is expanded
// back from end; end defaults to now. The span is clamped to usageMaxWindow.
func parseUsageWindow(q url.Values, now time.Time) (start, end time.Time, ok bool) {
	end = now
	if v := q.Get("end"); v != "" {
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		end = time.Unix(sec, 0).UTC()
	}

	if v := q.Get("start"); v != "" {
		sec, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		start = time.Unix(sec, 0).UTC()
	} else {
		span := usageDefaultSince
		if v := q.Get("since"); v != "" {
			d, err := store.ParseSince(v)
			if err != nil {
				return time.Time{}, time.Time{}, false
			}
			span = d
		}
		start = end.Add(-span)
	}

	if !start.Before(end) {
		return time.Time{}, time.Time{}, false
	}
	if end.Sub(start) > usageMaxWindow {
		start = end.Add(-usageMaxWindow)
	}
	return start, end, true
}
