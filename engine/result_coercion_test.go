package engine

// IMPROVEMENT-PLAN 3.22. The result column is JSON-typed on every dialect, so a
// result that is not valid JSON cannot be stored and something has to give.
// Replacing it with "{}" is right -- failing the terminal write would lose a
// whole workflow over a formatting defect -- but it was done silently, in a
// two-line conditional with no log statement, in all three stores.
//
// That is how an ambiguous durable call was erased rather than mislabelled: the
// engine detected it, the guest reported it, the generated wrapper turned it
// into `{"error":""durable call ...""}` (doubled quotes, invalid), and this
// replaced the lot with `{}`. The workflow was stored `done` with no error
// anywhere.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestCoerceResultJSON(t *testing.T) {
	for _, tc := range []struct {
		name      string
		in        string
		want      string
		wantLog   bool
		wantInLog string
	}{
		{
			name: "valid JSON is passed through untouched",
			in:   `{"charged":true}`,
			want: `{"charged":true}`,
		},
		{
			name: "a JSON scalar is valid JSON and is kept",
			in:   `"completed"`,
			want: `"completed"`,
		},
		{
			name: "empty is the ordinary no-result case and is not logged",
			in:   "",
			want: "{}",
		},
		{
			// The exact shape 3.22 produced.
			name:      "invalid JSON is replaced and reported",
			in:        `{"error":""durable call payments.Ship: [0] [AMBIGUOUS] call outcome unknown""}`,
			want:      "{}",
			wantLog:   true,
			wantInLog: "AMBIGUOUS",
		},
		{
			name:      "a bare string is not JSON either",
			in:        `completed`,
			want:      "{}",
			wantLog:   true,
			wantInLog: "completed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

			got := coerceResultJSON(context.Background(), log, "wf-1", tc.in)
			if got != tc.want {
				t.Errorf("coerceResultJSON(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if !json.Valid([]byte(got)) {
				t.Errorf("coerceResultJSON returned %q, which is not valid JSON -- the column will reject it", got)
			}

			logged := buf.String()
			if tc.wantLog {
				if logged == "" {
					t.Error("a result was discarded with no log line: this is the silence that erased 3.22's " +
						"ambiguity, and the reason the workflow read as a clean success")
				}
				if tc.wantInLog != "" && !strings.Contains(logged, tc.wantInLog) {
					t.Errorf("the log line does not carry what was discarded (%q); it must, or the "+
						"information is still gone: %s", tc.wantInLog, logged)
				}
				if !strings.Contains(logged, "wf-1") {
					t.Errorf("the log line does not name the workflow: %s", logged)
				}
			} else if logged != "" {
				t.Errorf("a valid or empty result was logged as a problem: %s", logged)
			}
		})
	}
}

// TestCoerceResultJSON_TruncatesLargeResults keeps a caller-controlled value
// from filling the log. A workflow result has no size limit worth relying on.
func TestCoerceResultJSON_TruncatesLargeResults(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	huge := strings.Repeat("x", 100_000) // not JSON
	if got := coerceResultJSON(context.Background(), log, "wf-2", huge); got != "{}" {
		t.Fatalf("got %q, want {}", got)
	}
	if n := buf.Len(); n > 4096 {
		t.Errorf("the log line is %d bytes: a 100 KB result was not truncated", n)
	}
	if !strings.Contains(buf.String(), "truncated") {
		t.Error("the log line does not say it was truncated, so the reader cannot tell a cut value " +
			"from a complete one")
	}
}

// TestCoerceResultJSON_NilLoggerIsSafe: the stores pass s.log(), which is
// non-nil in production, but a zero-value store in a test is not worth a panic.
func TestCoerceResultJSON_NilLoggerIsSafe(t *testing.T) {
	if got := coerceResultJSON(context.Background(), nil, "wf-3", "not json"); got != "{}" {
		t.Errorf("got %q, want {}", got)
	}
}
