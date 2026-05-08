// Event-driven user signup workflow triggered by a domain event.
//
// Demonstrates:
//   - Triggered by POST /api/events/publish (event type "user.signup")
//   - Receiving structured event data as workflow input
//   - DurableCall for outbound side effects (welcome email)
//   - SetQueryState for external status tracking
//   - AwaitEvent (via host function) to wait for a follow-up event
//
// Build:
//
//	cleat build -o /tmp/out ./examples/event-driven/
//
// Run worker with event triggers:
//
//	cleat-worker --db "postgres://..." --api-addr :8080
//
// Create a subscription:
//
//	curl -X POST http://localhost:8080/api/events/subscriptions \
//	  -H "Content-Type: application/json" \
//	  -H "X-Tenant-ID: <tenant-uuid>" \
//	  -d '{
//	    "event_type": "user.signup",
//	    "def_name": "event-driven",
//	    "entry_point": "HandleSignup",
//	    "input_template": {
//	      "user_id": "{{.event.data.user_id}}",
//	      "email": "{{.event.data.email}}",
//	      "name": "{{.event.data.name}}"
//	    }
//	  }'
//
// Publish a signup event:
//
//	curl -X POST http://localhost:8080/api/events/publish \
//	  -H "Content-Type: application/json" \
//	  -H "X-Tenant-ID: <tenant-uuid>" \
//	  -d '{
//	    "id": "unique-event-id",
//	    "event_type": "user.signup",
//	    "data": {
//	      "user_id": "usr_abc123",
//	      "email": "alice@example.com",
//	      "name": "Alice Smith"
//	    }
//	  }'
//
// Check event status:
//
//	curl http://localhost:8080/api/events/publish/<event-id>/status
package eventdriven

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rcownie/cleat/durable"
)

var h cleat.HostCalls

// ---- Domain types ----

type SignupInput struct {
	UserID string `json:"user_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
}

type SignupResult struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	WelcomeSent bool   `json:"welcome_sent"`
	Profile     ProfileInfo `json:"profile"`
}

type ProfileInfo struct {
	DisplayName string `json:"display_name"`
	JoinedAt    string `json:"joined_at"`
}

type WelcomeEmailResponse struct {
	MessageID string `json:"message_id"`
	Sent      bool   `json:"sent"`
}

// ---- Entry point ----

func HandleSignup(h cleat.HostCalls, input SignupInput) (*SignupResult, error) {
	if input.UserID == "" || input.Email == "" {
		return nil, fmt.Errorf("user_id and email are required")
	}

	h.SetQueryState("stage", "processing")
	h.SetQueryState("user_id", input.UserID)
	h.SetQueryState("email", input.Email)
	h.DurableLog(fmt.Sprintf("Processing signup: user=%s email=%s", input.UserID, input.Email))

	// Step 1: Create user profile via backing service.
	profileResp, err := h.DurableCall("users", "CreateProfile", toJSON(map[string]interface{}{
		"user_id": input.UserID,
		"email":   input.Email,
		"name":    input.Name,
	}))
	if err != nil {
		h.SetQueryState("stage", "profile_failed")
		return nil, fmt.Errorf("create profile failed: %w", err)
	}

	var profile ProfileInfo
	if err := json.Unmarshal([]byte(profileResp), &profile); err != nil {
		profile = ProfileInfo{
			DisplayName: input.Name,
			JoinedAt:    time.Now().Format(time.RFC3339),
		}
	}

	h.SetQueryState("stage", "sending_welcome")
	h.DurableLog(fmt.Sprintf("Profile created: user=%s", input.UserID))

	// Step 2: Send welcome email (best-effort).
	welcomeResp, err := h.DurableCall("email", "SendWelcome", toJSON(map[string]interface{}{
		"user_id":     input.UserID,
		"email":       input.Email,
		"name":        input.Name,
		"template":    "welcome_onboarding",
	}))
	welcomeSent := err == nil

	if err == nil {
		var wr WelcomeEmailResponse
		json.Unmarshal([]byte(welcomeResp), &wr)
	} else {
		h.DurableLog(fmt.Sprintf("Welcome email send failed (best-effort): %v", err))
	}

	// Step 3: Wait for a "user.activated" follow-up event (demonstrates AwaitEvent).
	// The workflow suspends here until the user activates or a timeout occurs.
	h.SetQueryState("stage", "awaiting_activation")
	h.DurableLog("Waiting for user activation event (user.activated)")

	// In production, this would call the await_event host function registered by
	// the event-triggers plugin.  For now, we simulate this with AwaitSignals
	// using the signal name pattern that handlePublishEvent broadcasts.
	signalResult := h.AwaitSignals([]string{"__evt:user.activated"}, 7*24*time.Hour)

	activationStatus := "timed_out"
	var activationPayload struct {
		ActivatedAt string `json:"activated_at"`
	}
	if !signalResult.TimedOut {
		json.Unmarshal([]byte(signalResult.Payload), &activationPayload)
		activationStatus = fmt.Sprintf("activated at %s", activationPayload.ActivatedAt)
		h.DurableLog(fmt.Sprintf("User activated: %s", input.UserID))
	} else {
		h.DurableLog(fmt.Sprintf("Activation timeout for user: %s", input.UserID))
	}

	h.SetQueryState("stage", "complete")
	h.SetQueryState("activation", activationStatus)
	h.SetQueryState("completed_at", h.Now().String())

	h.DurableLog(fmt.Sprintf("Signup complete: user=%s welcome=%v activation=%s",
		input.UserID, welcomeSent, activationStatus))

	return &SignupResult{
		UserID:      input.UserID,
		Email:       input.Email,
		WelcomeSent: welcomeSent,
		Profile:     profile,
	}, nil
}

func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
