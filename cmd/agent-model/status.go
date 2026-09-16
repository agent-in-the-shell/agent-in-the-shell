package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// gatewayHealth is the outcome of probing the running gateway's /healthz.
type gatewayHealth struct {
	Up     bool
	Detail string
}

// gatewayProber probes the gateway reachable at listen (a config `listen`
// value). It is an injection seam so tests never bind a real port.
type gatewayProber func(listen string) gatewayHealth

// doStatus implements `agent-model status`: a read-only, offline health
// snapshot of the gateway, every token store the config references, and the
// model routing table. It exits non-zero (returns a non-nil error) when the
// gateway is down or any deployment referenced by model_list is unusable, so it
// can back a health probe.
func doStatus(args []string, stdout, stderr io.Writer) error {
	return doStatusWith(args, stdout, stderr, probeGateway)
}

func doStatusWith(args []string, stdout, stderr io.Writer, probe gatewayProber) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to YAML config (default ~/.config/agentmodel/config.yaml)")
	jsonOut := fs.Bool("json", false, "emit JSON instead of a table")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := agentmodel.LoadConfig(*configPath)
	if err != nil {
		return err
	}

	rep := buildStatusReport(cfg, probe)

	if *jsonOut {
		if err := writeStatusJSON(stdout, rep); err != nil {
			return err
		}
	} else {
		writeStatusTable(stdout, rep)
	}

	if !rep.Healthy {
		return fmt.Errorf("unhealthy: %d problem(s)", rep.Problems)
	}
	return nil
}

// --- report model -----------------------------------------------------------

type storeRow struct {
	Kind        string // "oauth" | "apikey"
	Provider    string
	Dir         string // oauth
	Env         string // apikey
	Status      string // OK | EXPIRED | MISSING | NO TOKEN | UNSET
	Detail      string // "expires <ts>" | "expired <ts>" | "expiry unknown" | "env NAME set/unset"
	ExpiresAtMS int64
	Expired     bool
	OK          bool
}

type routeRow struct {
	Model    string
	Provider string
	Backend  string // provider/model
	Status   string
}

type statusReport struct {
	Listen   string
	Gateway  gatewayHealth
	Stores   []storeRow
	Routing  []routeRow
	Healthy  bool
	Problems int
}

// credRef identifies one credential a deployment depends on.
type credRef struct {
	kind     string
	provider string
	dir      string
	env      string
}

func (c credRef) key() string { return c.kind + "|" + c.provider + "|" + c.dir + "|" + c.env }

// deploymentCreds enumerates the credentials a deployment relies on: OAuth token
// directories (single or pooled) and/or API-key env vars.
func deploymentCreds(d agentmodel.DeploymentConfig) []credRef {
	var creds []credRef
	if d.OAuthTokenDir != "" {
		creds = append(creds, credRef{kind: "oauth", provider: d.Provider, dir: d.OAuthTokenDir})
	}
	for _, dir := range d.OAuthTokenDirs {
		creds = append(creds, credRef{kind: "oauth", provider: d.Provider, dir: dir})
	}
	if d.APIKeyEnv != "" {
		creds = append(creds, credRef{kind: "apikey", provider: d.Provider, env: d.APIKeyEnv})
	}
	for _, env := range d.APIKeyEnvs {
		creds = append(creds, credRef{kind: "apikey", provider: d.Provider, env: env})
	}
	return creds
}

func buildStatusReport(cfg *agentmodel.Config, probe gatewayProber) statusReport {
	rep := statusReport{Listen: cfg.Listen, Gateway: probe(cfg.Listen)}

	rows := make(map[string]storeRow)
	var order []string
	inspect := func(c credRef) storeRow {
		if row, ok := rows[c.key()]; ok {
			return row
		}
		row := inspectCred(c)
		rows[c.key()] = row
		order = append(order, c.key())
		return row
	}

	for _, m := range cfg.ModelList {
		for _, d := range m.Deployments {
			// weight: 0 is disabled — the factory drops it, so it never serves.
			// Don't report it as a routable deployment.
			if d.EffectiveWeight() == 0 {
				continue
			}
			creds := deploymentCreds(d)
			// Routing verdict is the worst credential verdict for the deployment.
			verdict := "OK"
			for _, c := range creds {
				row := inspect(c)
				if !row.OK && verdict == "OK" {
					verdict = row.Status
				}
			}
			rep.Routing = append(rep.Routing, routeRow{
				Model:    m.ModelName,
				Provider: d.Provider,
				Backend:  d.Provider + "/" + d.Model,
				Status:   verdict,
			})
		}
	}

	for _, k := range order {
		row := rows[k]
		rep.Stores = append(rep.Stores, row)
		if !row.OK {
			rep.Problems++
		}
	}
	if !rep.Gateway.Up {
		rep.Problems++
	}
	rep.Healthy = rep.Problems == 0
	return rep
}

func inspectCred(c credRef) storeRow {
	row := storeRow{Kind: c.kind, Provider: c.provider, Dir: c.dir, Env: c.env}
	if c.kind == "apikey" {
		if os.Getenv(c.env) != "" {
			row.Status, row.OK, row.Detail = "OK", true, "env "+c.env+" set"
		} else {
			row.Status, row.OK, row.Detail = "UNSET", false, "env "+c.env+" unset"
		}
		return row
	}

	st, err := auth.InspectStore(c.provider, c.dir)
	if err != nil {
		row.Status, row.OK, row.Detail = "ERROR", false, err.Error()
		return row
	}
	row.ExpiresAtMS, row.Expired = st.ExpiresAtMS, st.Expired
	switch {
	case !st.Exists:
		row.Status, row.OK, row.Detail = "MISSING", false, "no auth.json"
	case !st.HasToken:
		row.Status, row.OK, row.Detail = "NO TOKEN", false, "no access token"
	case !st.GatewayReadable:
		// A token is present but in a format the gateway's request path can't
		// read (nested Claude-CLI/pi layout) — it is unusable regardless of its
		// embedded expiry. Report it truthfully rather than a false OK/EXPIRED.
		row.Status, row.OK, row.Detail = "WRONG-FMT", false, "gateway can't read "+st.Format+" format (re-login to native)"
	case st.Expired:
		row.Status, row.OK, row.Detail = "EXPIRED", false, "expired "+fmtMS(st.ExpiresAtMS)
	case st.ExpiresAtMS == 0:
		row.Status, row.OK, row.Detail = "OK", true, "expiry unknown"
	default:
		row.Status, row.OK, row.Detail = "OK", true, "expires "+fmtMS(st.ExpiresAtMS)
	}
	return row
}

// fmtMS renders a unix-millisecond expiry in local time; 0 is "-".
func fmtMS(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

// --- rendering ---------------------------------------------------------------

func writeStatusTable(w io.Writer, rep statusReport) {
	upWord := "down"
	if rep.Gateway.Up {
		upWord = "up"
	}
	fmt.Fprintf(w, "gateway    %s   %s (%s)\n\n", rep.Listen, upWord, rep.Gateway.Detail)

	fmt.Fprintln(w, "token stores:")
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, s := range rep.Stores {
		loc := s.Dir
		if s.Kind == "apikey" {
			loc = "env " + s.Env
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", s.Provider, loc, s.Status, s.Detail)
	}
	tw.Flush()

	fmt.Fprintln(w, "\nmodel routing:")
	rtw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, r := range rep.Routing {
		fmt.Fprintf(rtw, "  %s\t-> %s\t%s\n", r.Model, r.Backend, r.Status)
	}
	rtw.Flush()

	if rep.Healthy {
		fmt.Fprintln(w, "\nhealthy")
	} else {
		fmt.Fprintf(w, "\nunhealthy: %d problem(s)\n", rep.Problems)
	}
}

func writeStatusJSON(w io.Writer, rep statusReport) error {
	type storeJSON struct {
		Provider    string `json:"provider"`
		Kind        string `json:"kind"`
		Dir         string `json:"dir,omitempty"`
		Env         string `json:"env,omitempty"`
		Status      string `json:"status"`
		Detail      string `json:"detail"`
		ExpiresAtMS int64  `json:"expires_at_ms,omitempty"`
		Expired     bool   `json:"expired"`
	}
	type routeJSON struct {
		Model    string `json:"model"`
		Provider string `json:"provider"`
		Backend  string `json:"backend"`
		Status   string `json:"status"`
	}
	out := struct {
		Gateway struct {
			Listen string `json:"listen"`
			Up     bool   `json:"up"`
			Detail string `json:"detail"`
		} `json:"gateway"`
		Stores   []storeJSON `json:"stores"`
		Routing  []routeJSON `json:"routing"`
		Healthy  bool        `json:"healthy"`
		Problems int         `json:"problems"`
	}{}
	out.Gateway.Listen = rep.Listen
	out.Gateway.Up = rep.Gateway.Up
	out.Gateway.Detail = rep.Gateway.Detail
	for _, s := range rep.Stores {
		out.Stores = append(out.Stores, storeJSON{
			Provider: s.Provider, Kind: s.Kind, Dir: s.Dir, Env: s.Env,
			Status: s.Status, Detail: s.Detail, ExpiresAtMS: s.ExpiresAtMS, Expired: s.Expired,
		})
	}
	for _, r := range rep.Routing {
		// routeJSON is routeRow plus json tags, so a conversion says "same shape,
		// different encoding" without relisting the fields — and a field added to
		// routeRow then fails to compile here instead of being silently dropped.
		out.Routing = append(out.Routing, routeJSON(r))
	}
	out.Healthy = rep.Healthy
	out.Problems = rep.Problems

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// --- gateway probe -----------------------------------------------------------

// probeGateway performs a best-effort liveness check against the gateway's
// /healthz. Any transport error or non-200 is reported as down; it never
// returns an error (a host may legitimately not be running the gateway).
func probeGateway(listen string) gatewayHealth {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(healthzURL(listen))
	if err != nil {
		return gatewayHealth{Up: false, Detail: "unreachable"}
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return gatewayHealth{Up: true, Detail: fmt.Sprintf("healthz %d", resp.StatusCode)}
	}
	return gatewayHealth{Up: false, Detail: fmt.Sprintf("healthz %d", resp.StatusCode)}
}

// healthzURL turns a config listen value (":8090", "0.0.0.0:8090",
// "127.0.0.1:8090") into a loopback /healthz URL to probe.
func healthzURL(listen string) string {
	hostport := listen
	if strings.HasPrefix(hostport, ":") {
		hostport = "127.0.0.1" + hostport
	} else if h, p, err := net.SplitHostPort(hostport); err == nil {
		if h == "" || h == "0.0.0.0" || h == "::" {
			h = "127.0.0.1"
		}
		hostport = net.JoinHostPort(h, p)
	}
	return "http://" + hostport + "/healthz"
}
