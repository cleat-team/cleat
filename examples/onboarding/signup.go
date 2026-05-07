// User onboarding — sign-up workflow with email verification and timeout.
//
// Demonstrates:
//   - AwaitSignals to wait for email verification click
//   - DurableSleep for timeout (24h verification window)
//   - SetQueryState for external status queries (check onboarding progress)
//   - Conditional branching based on signal vs timeout
//   - Saga-style cleanup on timeout (delete pending registration)
//
// Build:
//
//	cleat build -o /tmp/out ./examples/onboarding/
package onboarding

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/rcownie/cleat/cleat"
)

var h cleat.HostCalls

// ---- Domain types ----

type SignupInput struct {
	Email    string `json:"email"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

type Profile struct {
	UserID    string `json:"user_id"`
	Email     string `json:"email"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// ---- Entry point ----

func RegisterUser(h cleat.HostCalls, input SignupInput) (*Profile, error) {
	if input.Email == "" || input.Name == "" {
		return nil, fmt.Errorf("email and name are required")
	}

	// Step 1: Create pending registration.
	h.SetQueryState("stage", "pending_verification")
	h.SetQueryState("email", input.Email)

	resp, err := h.DurableCall("users", "CreatePendingRegistration", toJSON(input))
	if err != nil {
		return nil, fmt.Errorf("registration failed: %w", err)
	}
	var tempUser struct{ UserID string `json:"user_id"` }
	json.Unmarshal([]byte(resp), &tempUser)

	h.SetQueryState("user_id", tempUser.UserID)
	h.DurableLog(fmt.Sprintf("Pending registration: user=%s email=%s", tempUser.UserID, input.Email))

	// Step 2: Send verification email.
	h.DurableCall("email", "SendVerification", toJSON(map[string]string{
		"user_id": tempUser.UserID,
		"email":   input.Email,
		"name":    input.Name,
	}))

	// Step 3: Wait for email verification signal or timeout.
	h.SetQueryState("stage", "awaiting_verification")

	result := h.AwaitSignals([]string{"email_verified"}, 24*time.Hour)

	if result.TimedOut {
		// Verification window closed — clean up.
		return handleVerificationTimeout(h, tempUser.UserID, input)
	}

	// Signal received — extract verification token.
	var payload struct {
		Token string `json:"token"`
	}
	json.Unmarshal([]byte(result.Payload), &payload)

	// Step 4: Create user profile.
	h.SetQueryState("stage", "creating_profile")

	profileResp, err := h.DurableCall("users", "CreateProfile", toJSON(map[string]string{
		"user_id":            tempUser.UserID,
		"email":              input.Email,
		"name":               input.Name,
		"verification_token": payload.Token,
	}))
	if err != nil {
		return nil, fmt.Errorf("profile creation failed: %w", err)
	}

	var profile Profile
	json.Unmarshal([]byte(profileResp), &profile)

	// Step 5: Send welcome email (best-effort).
	h.DurableCall("email", "SendWelcome", toJSON(map[string]string{
		"user_id": tempUser.UserID,
		"email":   input.Email,
		"name":    input.Name,
	}))

	h.SetQueryState("stage", "complete")
	h.SetQueryState("completed_at", h.Now().String())
	h.DurableLog(fmt.Sprintf("Onboarding complete: user=%s", tempUser.UserID))

	return &profile, nil
}

func handleVerificationTimeout(h cleat.HostCalls, userID string, input SignupInput) (*Profile, error) {
	h.SetQueryState("stage", "timed_out")
	h.DurableLog(fmt.Sprintf("Verification timed out: user=%s", userID))

	// Send reminder (best-effort).
	h.DurableCall("email", "SendReminder", toJSON(map[string]string{
		"user_id": userID,
		"email":   input.Email,
	}))

	// Clean up pending registration.
	if _, err := h.DurableCall("users", "DeletePendingRegistration", toJSON(map[string]string{
		"user_id": userID,
	})); err != nil {
		h.DurableLog(fmt.Sprintf("Warning: failed to clean up pending registration: %v", err))
	}

	h.SetQueryState("stage", "canceled")
	return nil, fmt.Errorf("email verification timed out after 24h")
}

func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}
