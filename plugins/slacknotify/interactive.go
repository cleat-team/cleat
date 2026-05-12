package slacknotify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// slackInteractivePayload represents a Slack interactive message callback.
type slackInteractivePayload struct {
	Type        string          `json:"type"`
	CallbackID  string          `json:"callback_id"`
	TriggerID   string          `json:"trigger_id"`
	User        json.RawMessage `json:"user,omitempty"`
	Channel     json.RawMessage `json:"channel,omitempty"`
	Actions     json.RawMessage `json:"actions,omitempty"`
	ResponseURL string          `json:"response_url,omitempty"`
	Team        json.RawMessage `json:"team,omitempty"`
	Message     json.RawMessage `json:"message,omitempty"`
}

// handleInteractiveCallback receives Slack interactive payloads (button clicks, etc.),
// verifies the request, and delivers a signal to the relevant workflow.
//
// Callback ID convention used by workflows: wf:<workflow_id>:sig:<signal_name>
func (p *Plugin) handleInteractiveCallback(w http.ResponseWriter, r *http.Request) {
	// Read the raw body before ParseForm consumes it (needed for signature verification)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		p.writeError(w, http.StatusBadRequest, "cannot read body")
		return
	}
	r.Body = io.NopCloser(strings.NewReader(string(bodyBytes)))

	if err := r.ParseForm(); err != nil {
		p.writeError(w, http.StatusBadRequest, "invalid form data")
		return
	}

	// Verify Slack signing signature if configured
	if p.slackSigningSecret != "" {
		timestamp := r.Header.Get("X-Slack-Request-Timestamp")
		signature := r.Header.Get("X-Slack-Signature")
		if timestamp == "" || signature == "" {
			p.writeError(w, http.StatusUnauthorized, "missing Slack signature headers")
			return
		}

		// Reject requests older than 5 minutes
		ts, err := strconv.ParseInt(timestamp, 10, 64)
		if err != nil || time.Now().Unix()-ts > 300 {
			p.writeError(w, http.StatusUnauthorized, "stale request")
			return
		}

		basestring := fmt.Sprintf("v0:%s:%s", timestamp, string(bodyBytes))
		mac := hmac.New(sha256.New, []byte(p.slackSigningSecret))
		mac.Write([]byte(basestring))
		expected := "v0=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(signature), []byte(expected)) {
			p.writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}
	}

	// Parse the Slack payload (URL-encoded JSON in the "payload" form field)
	payloadStr := r.PostForm.Get("payload")
	if payloadStr == "" {
		p.writeError(w, http.StatusBadRequest, "missing payload")
		return
	}

	var payload slackInteractivePayload
	if err := json.Unmarshal([]byte(payloadStr), &payload); err != nil {
		p.writeError(w, http.StatusBadRequest, "invalid payload JSON: "+err.Error())
		return
	}

	// Extract workflow signal from callback_id
	// Convention: wf:<workflowID>:sig:<signalName>
	if payload.CallbackID == "" {
		// No callback_id — nothing to route. Return 200 OK per Slack requirements.
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	parts := strings.SplitN(payload.CallbackID, ":", 4)
	if len(parts) != 4 || parts[0] != "wf" || parts[2] != "sig" {
		p.logger.Warn("slack-notify: unrecognized callback_id format", "callback_id", payload.CallbackID)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
		return
	}

	wfID := parts[1]
	sigName := parts[3]

	// Deliver as workflow signal
	sigPayload, _ := json.Marshal(payload)
	if p.signalWorkflow != nil {
		if err := p.signalWorkflow(r.Context(), wfID, sigName, string(sigPayload)); err != nil {
			p.logger.Error("slack-notify: failed to deliver signal", "workflow_id", wfID, "signal", sigName, "error", err)
			p.writeError(w, http.StatusInternalServerError, "failed to deliver signal")
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}
