package webhookingest

import (
	"testing"

	sdk "github.com/cleat-team/cleat/cleat/pluginclients/webhookingest"
)

// The SDK's Wait suspends on "__evt:"+type, and the claim is keyed on an EXACT
// type, so when a caller names none it has to use the type this plugin assigns an
// event whose sender set neither X-Github-Event nor X-Event-Type. The SDK cannot
// import this package (it is a different module), so the two constants are
// compared here, where both are visible. A mismatch is a wait that never wakes
// and times out looking like a quiet source (cleat#3794).
func TestTheSDKWaitHelpersDefaultEventTypeIsThePluginsDefault(t *testing.T) {
	if sdk.DefaultEventType != defaultWebhookEventType {
		t.Fatalf("SDK Wait waits on %q when no event type is given, but the plugin stores such an event as %q",
			sdk.DefaultEventType, defaultWebhookEventType)
	}
}
