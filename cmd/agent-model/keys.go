package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

const defaultAgentModelBaseURL = "http://127.0.0.1:8080"

type keyCLIView struct {
	Revision       int64           `json:"revision,omitempty"`
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	KeyHash        string          `json:"key_hash"`
	Models         []string        `json:"models,omitempty"`
	MaxBudget      *float64        `json:"max_budget,omitempty"`
	BudgetDuration string          `json:"budget_duration,omitempty"`
	ExpiresAt      *int64          `json:"expires_at,omitempty"`
	Disabled       bool            `json:"disabled"`
	RevokedAt      *int64          `json:"revoked_at,omitempty"`
	Kind           string          `json:"kind"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
	CreatedAt      int64           `json:"created_at"`
	Spend          *float64        `json:"spend,omitempty"`
	Key            string          `json:"key,omitempty"`
}

type keyCLIList struct {
	Data []keyCLIView `json:"data"`
}

type keyCreateBody struct {
	Name           string          `json:"name"`
	Models         []string        `json:"models,omitempty"`
	MaxBudget      *float64        `json:"max_budget,omitempty"`
	BudgetDuration string          `json:"budget_duration,omitempty"`
	Duration       string          `json:"duration,omitempty"`
	Metadata       json.RawMessage `json:"metadata,omitempty"`
}

type keyCLICommon struct {
	baseURL string
	token   string
	jsonOut bool
}

func doKeys(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("keys requires a subcommand: create, list, show, disable, enable, revoke, delete, or rotate")
	}
	switch args[0] {
	case "create":
		return doKeysCreate(args[1:], stdout, stderr)
	case "list":
		return doKeysList(args[1:], stdout, stderr)
	case "show":
		return doKeysShow(args[1:], stdout, stderr)
	case "revoke", "disable", "enable":
		return doKeysToggle(args[0], args[1:], stdout, stderr)
	case "delete":
		return doKeysDelete(args[1:], stdout, stderr)
	case "rotate":
		return doKeysRotate(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown keys subcommand %q", args[0])
	}
}

func addKeyCommonFlags(fs *flag.FlagSet, stderr io.Writer) *keyCLICommon {
	fs.SetOutput(stderr)
	c := &keyCLICommon{}
	fs.StringVar(&c.baseURL, "base-url", envOr("AGENT_MODEL_BASE_URL", defaultAgentModelBaseURL), "agent-model server base URL")
	fs.StringVar(&c.token, "token", os.Getenv("AGENT_MODEL_TOKEN"), "master bearer token (default AGENT_MODEL_TOKEN)")
	fs.BoolVar(&c.jsonOut, "json", false, "print JSON")
	return c
}

func doKeysCreate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys create", flag.ContinueOnError)
	c := addKeyCommonFlags(fs, stderr)
	name := fs.String("name", "", "operator-facing key name (required)")
	models := fs.String("models", "", "comma-separated model allowlist (empty = all)")
	maxBudget := fs.Float64("max-budget", 0, "USD budget cap (empty = unlimited)")
	budgetDuration := fs.String("budget-duration", "", "budget window, e.g. 24h (empty = lifetime)")
	duration := fs.String("duration", "", "key lifetime, e.g. 720h (empty = never expires)")
	metadata := fs.String("metadata", "", "JSON metadata")
	tokenOnly := fs.Bool("token-only", false, "print only the new token to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	body, err := buildKeyCreateBody(*name, *models, *maxBudget, *budgetDuration, *duration, *metadata)
	if err != nil {
		return err
	}
	view, err := createManagedKey(c, body)
	if err != nil {
		return err
	}
	if *tokenOnly {
		fmt.Fprintln(stdout, view.Key)
		return nil
	}
	return printCreatedKey(stdout, view, c.jsonOut)
}

func buildKeyCreateBody(name, models string, maxBudget float64, budgetDuration, duration, metadata string) (keyCreateBody, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return keyCreateBody{}, fmt.Errorf("--name is required")
	}
	body := keyCreateBody{Name: name, Models: splitCSV(models), BudgetDuration: budgetDuration, Duration: duration}
	if maxBudget < 0 {
		return keyCreateBody{}, fmt.Errorf("--max-budget must be greater than 0")
	}
	if maxBudget > 0 {
		body.MaxBudget = &maxBudget
	}
	if metadata != "" {
		if !json.Valid([]byte(metadata)) {
			return keyCreateBody{}, fmt.Errorf("--metadata must be valid JSON")
		}
		body.Metadata = json.RawMessage(metadata)
	}
	return body, nil
}

func doKeysList(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys list", flag.ContinueOnError)
	c := addKeyCommonFlags(fs, stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	var list keyCLIList
	if err := keyRequest(c, http.MethodGet, "/v1/keys", nil, &list); err != nil {
		return err
	}
	if c.jsonOut {
		return writeCLIJSON(stdout, list)
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tSTATUS\tMODELS\tEXPIRES")
	for _, k := range list.Data {
		status := "active"
		if k.RevokedAt != nil {
			status = "revoked"
		} else if k.Disabled {
			status = "disabled"
		} else if k.ExpiresAt != nil && *k.ExpiresAt <= time.Now().Unix() {
			status = "expired"
		}
		modelText := "all"
		if len(k.Models) > 0 {
			modelText = strings.Join(k.Models, ",")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", k.Name, k.ID, status, modelText, formatUnix(k.ExpiresAt, "never"))
	}
	return tw.Flush()
}

func doKeysShow(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys show", flag.ContinueOnError)
	c := addKeyCommonFlags(fs, stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := oneKeyID(fs)
	if err != nil {
		return err
	}
	var view keyCLIView
	if err := keyRequest(c, http.MethodGet, "/v1/keys/"+id, nil, &view); err != nil {
		return err
	}
	if c.jsonOut {
		return writeCLIJSON(stdout, view)
	}
	return printKeyView(stdout, view)
}

func doKeysToggle(action string, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys "+action, flag.ContinueOnError)
	c := addKeyCommonFlags(fs, stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := oneKeyID(fs)
	if err != nil {
		return err
	}
	var view keyCLIView
	if err := keyRequest(c, http.MethodPost, "/v1/keys/"+id+"/"+action, nil, &view); err != nil {
		return err
	}
	if c.jsonOut {
		return writeCLIJSON(stdout, view)
	}
	verb := map[string]string{"revoke": "Revoked", "disable": "Disabled", "enable": "Enabled"}[action]
	fmt.Fprintf(stdout, "%s %s (%s)\n", verb, view.Name, view.ID)
	return nil
}

func doKeysDelete(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys delete", flag.ContinueOnError)
	c := addKeyCommonFlags(fs, stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := oneKeyID(fs)
	if err != nil {
		return err
	}
	if err := keyRequest(c, http.MethodDelete, "/v1/keys/"+id, nil, nil); err != nil {
		return err
	}
	if c.jsonOut {
		_, err = fmt.Fprintf(stdout, "{\"revoked\":%q}\n", id)
		return err
	}
	fmt.Fprintf(stdout, "Revoked %s (history retained)\n", id)
	return nil
}

func doKeysRotate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("keys rotate", flag.ContinueOnError)
	c := addKeyCommonFlags(fs, stderr)
	revokeOld := fs.Bool("revoke-old", false, "legacy: revoke the old key after creating its replacement; Service always invalidates the old secret immediately")
	tokenOnly := fs.Bool("token-only", false, "print only the new token to stdout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	id, err := oneKeyID(fs)
	if err != nil {
		return err
	}
	var old keyCLIView
	if err := keyRequest(c, http.MethodGet, "/v1/keys/"+id, nil, &old); err != nil {
		return err
	}
	if old.Kind == "portal" {
		return fmt.Errorf("generic rotation cannot preserve %s ownership; use the personal Portal for user keys", old.Kind)
	}
	if old.Kind == "service" {
		if old.Revision < 1 {
			return fmt.Errorf("service revision unavailable; upgrade server before rotation")
		}
		var view keyCLIView
		if err := keyRequest(c, http.MethodPost, "/v1/keys/"+id+"/rotate", map[string]int64{"revision": old.Revision}, &view); err != nil {
			return err
		}
		// Service rotation always invalidates the old secret, even without --revoke-old.
		if *tokenOnly {
			fmt.Fprintln(stdout, view.Key)
			return nil
		}
		return printCreatedKey(stdout, view, c.jsonOut)
	}
	if old.RevokedAt != nil {
		return fmt.Errorf("revoked keys cannot be rotated; create a replacement with explicit policy")
	}
	body := keyCreateBody{Name: old.Name, Models: old.Models, MaxBudget: old.MaxBudget, BudgetDuration: old.BudgetDuration, Metadata: rotationMetadata(old.Metadata, old.ID)}
	view, err := createManagedKey(c, body)
	if err != nil {
		return err
	}
	if *revokeOld {
		var ignored keyCLIView
		if err := keyRequest(c, http.MethodPost, "/v1/keys/"+id+"/revoke", nil, &ignored); err != nil {
			return fmt.Errorf("created replacement %s but could not revoke old key %s: %w", view.ID, id, err)
		}
	}
	if *tokenOnly {
		fmt.Fprintln(stdout, view.Key)
		return nil
	}
	if c.jsonOut {
		return writeCLIJSON(stdout, view)
	}
	if err := printCreatedKey(stdout, view, false); err != nil {
		return err
	}
	if !*revokeOld {
		fmt.Fprintf(stderr, "Old key %s remains active; revoke it after rollout.\n", id)
	}
	return nil
}

func createManagedKey(c *keyCLICommon, body keyCreateBody) (keyCLIView, error) {
	var view keyCLIView
	err := keyRequest(c, http.MethodPost, "/v1/keys", body, &view)
	return view, err
}

func keyRequest(c *keyCLICommon, method, path string, body, out any) error {
	if strings.TrimSpace(c.token) == "" {
		return fmt.Errorf("master token is required (--token or AGENT_MODEL_TOKEN)")
	}
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(c.baseURL, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func oneKeyID(fs *flag.FlagSet) (string, error) {
	if fs.NArg() != 1 {
		return "", fmt.Errorf("%s requires exactly one key ID", fs.Name())
	}
	return fs.Arg(0), nil
}
func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}
func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}
func writeCLIJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
func formatUnix(v *int64, fallback string) string {
	if v == nil {
		return fallback
	}
	return time.Unix(*v, 0).UTC().Format(time.RFC3339)
}
func printCreatedKey(w io.Writer, v keyCLIView, jsonOut bool) error {
	if jsonOut {
		return writeCLIJSON(w, v)
	}
	fmt.Fprintf(w, "Created virtual key\n\nID      %s\nName    %s\nToken   %s\n", v.ID, v.Name, v.Key)
	return nil
}
func printKeyView(w io.Writer, v keyCLIView) error {
	status := "active"
	if v.RevokedAt != nil {
		status = "revoked"
	} else if v.Disabled {
		status = "disabled"
	} else if v.ExpiresAt != nil && *v.ExpiresAt <= time.Now().Unix() {
		status = "expired"
	}
	models := "all"
	if len(v.Models) > 0 {
		models = strings.Join(v.Models, ", ")
	}
	fmt.Fprintf(w, "ID       %s\nName     %s\nStatus   %s\nModels   %s\nExpires  %s\nCreated  %s\n", v.ID, v.Name, status, models, formatUnix(v.ExpiresAt, "never"), time.Unix(v.CreatedAt, 0).UTC().Format(time.RFC3339))
	if v.Spend != nil {
		fmt.Fprintf(w, "Spend    $%.6f\n", *v.Spend)
	}
	return nil
}
func rotationMetadata(raw json.RawMessage, oldID string) json.RawMessage {
	m := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &m)
	}
	m["rotated_from"] = oldID
	b, _ := json.Marshal(m)
	return b
}
