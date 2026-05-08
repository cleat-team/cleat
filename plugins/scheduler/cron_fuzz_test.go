package scheduler

import (
	"testing"
	"time"
)

// FuzzCronParser fuzzes the cron expression parser with random strings.
// It exercises parseCron's field parsing logic including step patterns,
// ranges, comma-separated lists, and error paths.
func FuzzCronParser(f *testing.F) {
	// Seed corpus: valid cron expressions
	f.Add("* * * * *")
	f.Add("*/5 * * * *")
	f.Add("0 * * * *")
	f.Add("0 9 * * *")
	f.Add("0 9 * * 1-5")
	f.Add("30 10 * * *")
	f.Add("15,30,45 * * * *")
	f.Add("0 9-17 * * *")
	f.Add("1-10/2 * * * *")
	f.Add("0 0 1 1 *")
	f.Add("59 23 31 12 6")

	// Seed corpus: edge cases and invalid expressions
	f.Add("")
	f.Add("* * * *")
	f.Add("* * * * * *")
	f.Add("abc")
	f.Add("*/ * * * *")
	f.Add("-1 * * * *")
	f.Add("60 * * * *")
	f.Add("* 24 * * *")
	f.Add("* * 0 * *")
	f.Add("* * 32 * *")
	f.Add("* * * 0 *")
	f.Add("* * * 13 *")
	f.Add("* * * * -1")
	f.Add("* * * * 7")
	f.Add("1-5-10 * * * *")
	f.Add("1-5/0 * * * *")
	f.Add("1-5/-1 * * * *")
	f.Add("*/0 * * * *")
	f.Add("*/abc * * * *")
	f.Add("5-10/3 * * * *")
	f.Add("1,2,3,4,5 * * * *")
	f.Add(",,, * * * *")
	f.Add("1-5/a * * * *")
	f.Add("1-5 * * * *")
	f.Add("1/2 * * * *")

	f.Fuzz(func(t *testing.T, expr string) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("panic on input %q: %v", expr, r)
			}
		}()

		// parseCron may return an error — that's fine, but must not panic.
		ce, err := parseCron(expr)
		if err != nil {
			return
		}

		// If parsing succeeded, also exercise the matches method with
		// various times to ensure no panic on evaluation.
		times := []time.Time{
			time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC),
			time.Date(2025, 12, 31, 23, 59, 0, 0, time.UTC),
			time.Date(2025, 2, 28, 0, 0, 0, 0, time.UTC),
		}
		for _, tm := range times {
			ce.matches(tm)
		}

		// Also exercise nextRun with the same expression.
		_ = nextRun(expr, time.Date(2025, 1, 15, 10, 0, 0, 0, time.UTC))
	})
}
