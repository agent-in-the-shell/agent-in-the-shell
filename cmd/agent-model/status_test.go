package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// upProbe / downProbe are injectable gateway health stubs so the tests never
// bind a real port.
func upProbe(listen string) gatewayHealth { return gatewayHealth{Up: true, Detail: "healthz 200"} }
func downProbe(listen string) gatewayHealth {
	return gatewayHealth{Up: false, Detail: "connection refused"}
}

// writeStatusConfig writes a minimal valid config whose single subscription
// deployment points at tokenDir, and returns the config path.
func writeStatusConfig(t *testing.T, modelName, tokenDir string) string {
	t.Helper()
	body := "" +
		"listen: \":18099\"\n" +
		"model_list:\n" +
		"  - model_name: \"" + modelName + "\"\n" +
		"    deployments:\n" +
		"      - provider: \"chatgpt\"\n" +
		"        model: \"gpt-5.5\"\n" +
		"        auth_mode: \"subscription\"\n" +
		"        oauth_token_dir: \"" + tokenDir + "\"\n" +
		"        weight: 100\n"
	return writeConfigFile(t, body)
}

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// writeTokenStore drops auth.json into a fresh dir and returns the dir.
func writeTokenStore(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	return dir
}

const (
	freshNativeStore   = `{"schema":"agentmodel.oauth/v1","provider":"chatgpt","access_token":"tok","account_id":"acct","expires_at_ms":4102444800000}`
	expiredNativeStore = `{"schema":"agentmodel.oauth/v1","provider":"chatgpt","access_token":"tok","expires_at_ms":946684800000}`
)

func TestDoStatus_AllHealthy_Table_ExitsZero(t *testing.T) {
	cfg := writeStatusConfig(t, "claude-sonnet-4-5", writeTokenStore(t, freshNativeStore))
	var out, errOut bytes.Buffer
	if err := doStatusWith([]string{"--config", cfg}, &out, &errOut, upProbe); err != nil {
		t.Fatalf("healthy status must return nil error, got: %v", err)
	}
	s := out.String()
	for _, want := range []string{"claude-sonnet-4-5", "OK", "up"} {
		if !strings.Contains(s, want) {
			t.Errorf("table missing %q:\n%s", want, s)
		}
	}
}

func TestDoStatus_ExpiredStore_ExitsNonZero(t *testing.T) {
	cfg := writeStatusConfig(t, "claude-sonnet-4-5", writeTokenStore(t, expiredNativeStore))
	var out, errOut bytes.Buffer
	err := doStatusWith([]string{"--config", cfg}, &out, &errOut, upProbe)
	if err == nil {
		t.Fatal("an expired active deployment must return a non-nil error (exit 1)")
	}
	if !strings.Contains(out.String(), "EXPIRED") {
		t.Errorf("table should mark the store EXPIRED:\n%s", out.String())
	}
}

func TestDoStatus_GatewayDown_ExitsNonZero(t *testing.T) {
	cfg := writeStatusConfig(t, "claude-sonnet-4-5", writeTokenStore(t, freshNativeStore))
	var out, errOut bytes.Buffer
	err := doStatusWith([]string{"--config", cfg}, &out, &errOut, downProbe)
	if err == nil {
		t.Fatal("a down gateway must return a non-nil error (exit 1)")
	}
	if !strings.Contains(out.String(), "down") {
		t.Errorf("table should report the gateway down:\n%s", out.String())
	}
}

func TestDoStatus_MissingStore_ExitsNonZero(t *testing.T) {
	// Config points at a token dir with no auth.json.
	cfg := writeStatusConfig(t, "claude-sonnet-4-5", t.TempDir())
	var out, errOut bytes.Buffer
	err := doStatusWith([]string{"--config", cfg}, &out, &errOut, upProbe)
	if err == nil {
		t.Fatal("a missing store must return a non-nil error (exit 1)")
	}
	if !strings.Contains(out.String(), "MISSING") {
		t.Errorf("table should mark the store MISSING:\n%s", out.String())
	}
}

func TestDoStatus_JSON_HealthyShape(t *testing.T) {
	cfg := writeStatusConfig(t, "claude-sonnet-4-5", writeTokenStore(t, freshNativeStore))
	var out, errOut bytes.Buffer
	if err := doStatusWith([]string{"--config", cfg, "--json"}, &out, &errOut, upProbe); err != nil {
		t.Fatalf("healthy --json must return nil error, got: %v", err)
	}
	var rep struct {
		Gateway struct {
			Up bool `json:"up"`
		} `json:"gateway"`
		Healthy bool `json:"healthy"`
		Stores  []struct {
			Provider string `json:"provider"`
			Status   string `json:"status"`
		} `json:"stores"`
		Routing []struct {
			Model  string `json:"model"`
			Status string `json:"status"`
		} `json:"routing"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if !rep.Healthy {
		t.Errorf("Healthy: got false, want true")
	}
	if !rep.Gateway.Up {
		t.Errorf("Gateway.Up: got false, want true")
	}
	if len(rep.Stores) != 1 || rep.Stores[0].Status != "OK" {
		t.Errorf("stores: got %+v, want one OK store", rep.Stores)
	}
	if len(rep.Routing) != 1 || rep.Routing[0].Model != "claude-sonnet-4-5" {
		t.Errorf("routing: got %+v, want the one model", rep.Routing)
	}
}

func TestDoStatus_APIKeyUnset_ExitsNonZero(t *testing.T) {
	const envName = "STATUS_TEST_UNSET_KEY_XYZ"
	os.Unsetenv(envName)
	body := "" +
		"listen: \":18099\"\n" +
		"model_list:\n" +
		"  - model_name: \"gpt-4\"\n" +
		"    deployments:\n" +
		"      - provider: \"openai\"\n" +
		"        model: \"gpt-4o\"\n" +
		"        auth_mode: \"api_key\"\n" +
		"        api_key_env: \"" + envName + "\"\n" +
		"        weight: 100\n"
	cfg := writeConfigFile(t, body)
	var out, errOut bytes.Buffer
	err := doStatusWith([]string{"--config", cfg}, &out, &errOut, upProbe)
	if err == nil {
		t.Fatal("an unset api_key_env must return a non-nil error (exit 1)")
	}
	s := out.String()
	if !strings.Contains(s, envName) || !strings.Contains(s, "UNSET") {
		t.Errorf("table should flag the unset api key env %q as UNSET:\n%s", envName, s)
	}
}

func TestDoStatus_BadConfig_Errors(t *testing.T) {
	var out, errOut bytes.Buffer
	err := doStatusWith([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml")}, &out, &errOut, upProbe)
	if err == nil {
		t.Fatal("an unreadable config must return an error")
	}
}

// TestDoStatus_ForeignFormatStore_FlaggedWrongFmt: a store in the nested
// Claude-CLI/pi format has a token, but the gateway's readCreds cannot read it,
// so status must flag WRONG-FMT and exit 1 — not a false OK (#1490).
func TestDoStatus_ForeignFormatStore_FlaggedWrongFmt(t *testing.T) {
	dir := writeTokenStore(t, `{"anthropic":{"access":"tok","expires":4102444800000}}`)
	body := "" +
		"listen: \":18099\"\n" +
		"model_list:\n" +
		"  - model_name: \"claude-3-5-sonnet-latest\"\n" +
		"    deployments:\n" +
		"      - provider: \"anthropic\"\n" +
		"        model: \"claude-3-5-sonnet-latest\"\n" +
		"        auth_mode: \"subscription\"\n" +
		"        oauth_token_dir: \"" + dir + "\"\n" +
		"        weight: 100\n"
	cfg := writeConfigFile(t, body)
	var out, errOut bytes.Buffer
	err := doStatusWith([]string{"--config", cfg}, &out, &errOut, upProbe)
	if err == nil {
		t.Fatal("a gateway-unreadable store must return a non-nil error (exit 1)")
	}
	if !strings.Contains(out.String(), "WRONG-FMT") {
		t.Errorf("table should flag the store WRONG-FMT:\n%s", out.String())
	}
}

// TestDoStatus_SkipsWeightZeroDeployment: a weight:0 (disabled) deployment is
// dropped by the factory and never serves, so status must not report it as a
// routable row (#1494 weight:0 semantics leaking into status).
func TestDoStatus_SkipsWeightZeroDeployment(t *testing.T) {
	fresh := writeTokenStore(t, freshNativeStore)
	disabled := writeTokenStore(t, freshNativeStore)
	body := "" +
		"listen: \":18099\"\n" +
		"model_list:\n" +
		"  - model_name: \"m\"\n" +
		"    deployments:\n" +
		"      - provider: \"chatgpt\"\n        model: \"gpt-5.5\"\n        auth_mode: \"subscription\"\n" +
		"        oauth_token_dir: \"" + fresh + "\"\n        weight: 1\n" +
		"      - provider: \"chatgpt\"\n        model: \"gpt-5.5\"\n        auth_mode: \"subscription\"\n" +
		"        oauth_token_dir: \"" + disabled + "\"\n        weight: 0\n"
	cfg := writeConfigFile(t, body)
	var out, errOut bytes.Buffer
	if err := doStatusWith([]string{"--config", cfg, "--json"}, &out, &errOut, upProbe); err != nil {
		t.Fatalf("doStatus: %v", err)
	}
	var rep struct {
		Routing []struct{ Model string } `json:"routing"`
	}
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("json: %v", err)
	}
	if len(rep.Routing) != 1 {
		t.Errorf("routing rows = %d, want 1 (the weight:0 deployment must be skipped)", len(rep.Routing))
	}
}
