package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/cleat-team/cleat/plugin"
)

// cleat#1596 stage 2, at the boundary that matters most in practice: a model
// call is the outbound hop an agent workflow makes, and it was the largest
// group of sites where a customer's trace ended.
//
// THIS ASSERTS THE HEADER A REAL SERVER RECEIVED, not that a helper was called.
// The repo guard (plugin/every_outbound_call_joins_the_trace_test.go) checks
// that SetTraceparent appears in each function -- a static fact, and one that a
// call passing the wrong context would satisfy while propagating nothing. The
// static check makes the sweep finishable; this one makes it true.
//
// THE TRACE REACHES A PROVIDER THROUGH CallContext AND NOTHING ELSE. These
// functions take no trace argument; the engine puts it on the call context
// (engine/plugin_call_context.go) and plugin.SetTraceparentFromContext unwraps
// it. So a provider given a bare context.Background() must send NO header --
// which is the second case below, and the one that would catch a "fix" that
// fabricates a trace when none is available.
const provTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

var provTPRE = regexp.MustCompile(`^00-([0-9a-f]{32})-([0-9a-f]{16})-0[01]$`)

func TestAProviderCallCarriesTheCallersTrace(t *testing.T) {
	// One row per provider entry point that a workflow can reach. Each is a
	// separately hand-written request in this repo, so a sweep that missed one
	// would be invisible without naming them individually.
	for _, tc := range []struct {
		name string
		call func(ctx context.Context, c *http.Client, base string) error
	}{
		{"openai chat", func(ctx context.Context, c *http.Client, base string) error {
			_, err := OpenAIChat(ctx, c, "k", base, ChatInput{Model: "m"})
			return err
		}},
		{"openai embed", func(ctx context.Context, c *http.Client, base string) error {
			_, err := OpenAIEmbed(ctx, c, "k", base, EmbedInput{Model: "m"})
			return err
		}},
		{"anthropic chat", func(ctx context.Context, c *http.Client, base string) error {
			_, err := AnthropicChat(ctx, c, "k", base, ChatInput{Model: "m"})
			return err
		}},
		{"gemini chat", func(ctx context.Context, c *http.Client, base string) error {
			_, err := GeminiChat(ctx, c, "k", base, ChatInput{Model: "m"})
			return err
		}},
		{"ollama chat", func(ctx context.Context, c *http.Client, base string) error {
			_, err := OllamaChat(ctx, c, base, ChatInput{Model: "m"})
			return err
		}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			var got string
			var seen bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, seen = r.Header.Get("traceparent"), true
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(200)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()

			ctx := plugin.WithCallContext(context.Background(),
				&plugin.CallContext{TraceID: provTraceID})
			// The provider's own error is not the subject -- a stub server
			// returning "{}" will not satisfy every decoder. What matters is
			// the header it saw on the way in.
			_ = tc.call(ctx, srv.Client(), srv.URL)

			if !seen {
				t.Fatal("the provider never reached the server, so this case measured nothing")
			}
			m := provTPRE.FindStringSubmatch(got)
			if m == nil {
				t.Fatalf("no usable traceparent reached the model endpoint (got %q) -- the "+
					"customer's trace ends at this hop and cleat looks like a leaf that "+
					"swallowed the model call (cleat#1596)", got)
			}
			if m[1] != provTraceID {
				t.Errorf("propagated trace-id %q, want %q -- a DIFFERENT id is the exact defect: "+
					"both ends look healthy and the chain is still broken", m[1], provTraceID)
			}
		})
	}
}

// THE CONTROL. A provider called outside a workflow -- no CallContext -- must
// send nothing. Without this, a version that fabricates a trace id whenever one
// is absent passes every case above, and a fabricated trace is worse than none
// because a collector believes it.
func TestAProviderWithNoCallContextSendsNoTrace(t *testing.T) {
	var got string
	var seen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, seen = r.Header.Get("traceparent"), true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	_, _ = OpenAIChat(context.Background(), srv.Client(), "k", srv.URL, ChatInput{Model: "m"})
	if !seen {
		t.Fatal("the provider never reached the server, so this measured nothing")
	}
	if got != "" {
		t.Errorf("sent traceparent %q with no CallContext -- inventing a trace is worse than "+
			"sending none, because a collector believes it and shows an operator a tree that "+
			"never existed", got)
	}
}
