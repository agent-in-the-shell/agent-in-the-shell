package api

import (
	"encoding/json"
	"net/http"

	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel"
	"github.com/agent-in-the-shell/agent-in-the-shell/services/agentmodel/auth"
)

// chatgptOAuthStart kicks off a device-code OAuth flow for a ChatGPT
// subscription. Returns the user_code, verification_uri, etc. that the
// caller should present to the human end-user. They visit the URL in a
// browser, type the user_code, then the caller polls /v1/oauth/chatgpt/poll.
func (s *Server) chatgptOAuthStart(w http.ResponseWriter, r *http.Request) {
	if s.chatgptAuth == nil {
		writeError(w, http.StatusNotFound, &agentmodel.Error{Type: agentmodel.ErrTypeNotFound, Message: "ChatGPT subscription auth is not configured"})
		return
	}

	challenge, err := s.chatgptAuth.LoginDeviceCode(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, &agentmodel.Error{Type: agentmodel.ErrTypeUpstream, Message: "device code start failed: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"device_code":      challenge.DeviceCode,
		"device_auth_id":   challenge.DeviceAuthID,
		"user_code":        challenge.UserCode,
		"verification_uri": challenge.VerificationURI,
		"expires_in":       challenge.ExpiresIn,
		"interval":         challenge.Interval,
	})
}

// chatgptOAuthPoll polls the device-code completion. Caller invokes this
// repeatedly (with the device_code from /start) until they get status
// "complete" or "expired".
//
// In our implementation PollDeviceCode blocks until completion or timeout,
// so a single call is enough. Callers should use a longer HTTP client
// timeout (15+ minutes) to accommodate.
func (s *Server) chatgptOAuthPoll(w http.ResponseWriter, r *http.Request) {
	if s.chatgptAuth == nil {
		writeError(w, http.StatusNotFound, &agentmodel.Error{Type: agentmodel.ErrTypeNotFound, Message: "ChatGPT subscription auth is not configured"})
		return
	}
	var body struct {
		DeviceCode   string `json:"device_code"`
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeInvalidJSON, Message: "invalid JSON: " + err.Error()})
		return
	}
	if body.DeviceCode == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "device_code", Message: "device_code is required"})
		return
	}
	if body.DeviceAuthID == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "device_auth_id", Message: "device_auth_id is required"})
		return
	}
	if body.UserCode == "" {
		writeError(w, http.StatusBadRequest, &agentmodel.Error{Type: agentmodel.ErrTypeInvalidRequest, Code: agentmodel.CodeMissingRequiredParam, Param: "user_code", Message: "user_code is required"})
		return
	}

	if err := s.chatgptAuth.PollDeviceCode(r.Context(), auth.DeviceCodeChallenge{
		DeviceCode:   body.DeviceCode,
		DeviceAuthID: body.DeviceAuthID,
		UserCode:     body.UserCode,
	}); err != nil {
		writeError(w, http.StatusBadGateway, &agentmodel.Error{Type: agentmodel.ErrTypeUpstream, Message: "device code poll failed: " + err.Error()})
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "complete"})
}
