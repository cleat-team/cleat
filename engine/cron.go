package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron expression parsing and evaluation.
//
// Moved here from store_promises.go, which is about promises and had acquired
// the cron evaluator for no reason other than that NextCronTime was written
// next to it.
//
// The dialect is five space-separated fields:
//
//	minute   hour   day-of-month   month   day-of-week
//	0-59     0-23   1-31           1-12    0-7 (0 and 7 are both Sunday)
//
// Each field is a comma-separated list of terms, where a term is one of
// `*`, `*/step`, `value`, `low-high`, or `low-high/step`.
//
// Not supported, deliberately: `@daily` and friends, three-letter names
// (MON, JAN), `L`/`W`/`#` modifiers, and second-level granularity. A schedule
// using any of them is rejected by ValidateCronExpr rather than silently
// misinterpreted -- which is the whole point of this file having a parser at
// all. See the note on ValidateCronExpr about why rejection is the safe
// direction here.

// cronExpr is a parsed five-field cron expression.
type cronExpr struct {
	minute cronField
	hour   cronField
	dom    cronField
	month  cronField
	dow    cronField
}

// cronField is one parsed field: the set of values it admits, plus whether it
// was written as a star. The star flag is not derivable from the value set --
// `*` and `0-59` admit the same minutes but differ for the day-of-month /
// day-of-week rule in cronExpr.dayMatches.
type cronField struct {
	star bool
	vals map[int]bool
}

func (f cronField) matches(v int) bool { return f.vals[v] }

// ValidateCronExpr reports whether expr is a cron expression this engine can
// evaluate, returning nil if it is and a diagnostic naming the offending field
// if it is not.
//
// This exists because the alternative was worse than useless. NextCronTime
// answers with a time.Time and has no way to say "that expression is
// nonsense", so it used to fall back to "24 hours from now" for anything it
// could not parse -- and its integer parser was fmt.Sscanf with the error
// discarded, so `abc` parsed as 0. `abc * * * *` therefore did not fail, and
// did not fall back either: it ran at minute zero of every hour, forever,
// having never told anyone. Rejecting up front is the only way a caller can
// report the problem to whoever typed it.
func ValidateCronExpr(expr string) error {
	_, err := parseCronExpr(expr)
	return err
}

func parseCronExpr(expr string) (cronExpr, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return cronExpr{}, fmt.Errorf(
			"cron: %q has %d field(s), want 5: minute hour day-of-month month day-of-week",
			expr, len(fields))
	}

	var (
		out cronExpr
		err error
	)
	if out.minute, err = parseCronField(fields[0], 0, 59, "minute"); err != nil {
		return cronExpr{}, err
	}
	if out.hour, err = parseCronField(fields[1], 0, 23, "hour"); err != nil {
		return cronExpr{}, err
	}
	if out.dom, err = parseCronField(fields[2], 1, 31, "day-of-month"); err != nil {
		return cronExpr{}, err
	}
	if out.month, err = parseCronField(fields[3], 1, 12, "month"); err != nil {
		return cronExpr{}, err
	}
	// Day-of-week admits 0-7 so that both 0 and 7 mean Sunday, which is the
	// usual convention and the one people reach for when writing "Sunday" by
	// hand. The previous evaluator capped the field at 6 and compared against
	// time.Weekday(), so `* * * * 7` matched nothing and the schedule simply
	// never fired.
	if out.dow, err = parseCronField(fields[4], 0, 7, "day-of-week"); err != nil {
		return cronExpr{}, err
	}
	if out.dow.vals[7] {
		out.dow.vals[0] = true
		delete(out.dow.vals, 7)
	}
	return out, nil
}

// parseCronField parses one comma-separated field into the set of values it
// admits. name is used only for diagnostics.
func parseCronField(pattern string, min, max int, name string) (cronField, error) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		return cronField{}, fmt.Errorf("cron: %s field is empty", name)
	}

	// Vixie cron sets the "star" flag from the field's leading character, so
	// `*/2` counts as a star for the day-of-month / day-of-week rule even
	// though it restricts which days match. robfig/cron does the same. Matching
	// the established implementations matters more here than the reading a
	// first-principles argument would give.
	f := cronField{star: strings.HasPrefix(pattern, "*"), vals: make(map[int]bool)}

	for _, term := range strings.Split(pattern, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			return cronField{}, fmt.Errorf("cron: %s field %q has an empty term", name, pattern)
		}

		step := 1
		if slash := strings.Index(term, "/"); slash >= 0 {
			stepStr := strings.TrimSpace(term[slash+1:])
			term = strings.TrimSpace(term[:slash])
			n, err := strconv.Atoi(stepStr)
			if err != nil {
				return cronField{}, fmt.Errorf("cron: %s field: step %q is not a number", name, stepStr)
			}
			if n < 1 {
				return cronField{}, fmt.Errorf("cron: %s field: step must be at least 1, got %d", name, n)
			}
			step = n
		}

		lo, hi := min, max
		switch {
		case term == "*":
			// Full range; lo/hi already set.
		case strings.Contains(term, "-"):
			// A range. Split on the first "-" only: none of these fields
			// admits a negative bound, so a second "-" is a malformed term
			// rather than a signed number.
			dash := strings.Index(term, "-")
			var err error
			if lo, err = cronAtoi(term[:dash], name); err != nil {
				return cronField{}, err
			}
			if hi, err = cronAtoi(term[dash+1:], name); err != nil {
				return cronField{}, err
			}
			if lo > hi {
				return cronField{}, fmt.Errorf(
					"cron: %s field: range %q is inverted (%d > %d)", name, term, lo, hi)
			}
		default:
			v, err := cronAtoi(term, name)
			if err != nil {
				return cronField{}, err
			}
			lo, hi = v, v
		}

		if lo < min || hi > max {
			return cronField{}, fmt.Errorf(
				"cron: %s field: %q is outside the allowed range %d-%d", name, term, min, max)
		}
		for v := lo; v <= hi; v += step {
			f.vals[v] = true
		}
	}

	return f, nil
}

func cronAtoi(s, name string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("cron: %s field: %q is not a number", name, strings.TrimSpace(s))
	}
	return n, nil
}

// dayMatches reports whether t falls on a day this expression admits.
//
// THIS IS THE RULE THAT USED TO BE WRONG, and it is worth stating in full
// because it is the one part of cron that does not do what reading the fields
// left to right suggests.
//
// Day-of-month and day-of-week are ORed when BOTH are restricted, not ANDed:
//
//	0 0 13 * 5     "midnight on the 13th, AND midnight every Friday"
//	               -- not "midnight on Friday the 13th"
//
// When only one of the two is restricted, that one alone decides, and when
// neither is, every day matches. POSIX specifies this, and Vixie cron, cronie
// and robfig/cron all implement it.
//
// The previous evaluator ANDed all five fields uniformly, so `0 0 13 * 5`
// fired on Friday the 13th -- a handful of times a year instead of roughly
// sixty. No test caught it: the only day-of-week test used `30 8,17 * * 1-5`,
// where day-of-month is `*`, and the two rules cannot disagree unless both
// fields are restricted.
func (e cronExpr) dayMatches(t time.Time) bool {
	if !e.month.matches(int(t.Month())) {
		return false
	}
	domOK := e.dom.matches(t.Day())
	dowOK := e.dow.matches(int(t.Weekday()))
	switch {
	case e.dom.star && e.dow.star:
		return true
	case e.dom.star:
		return dowOK
	case e.dow.star:
		return domOK
	default:
		return domOK || dowOK
	}
}

// NextCronTime computes the next firing time at or after from for a five-field
// cron expression.
//
// An expression that does not parse yields from+24h, preserving the behaviour
// callers have always had. That fallback is a poor answer and callers should
// prefer to reject the expression with ValidateCronExpr before it is ever
// stored -- but changing what this returns would silently re-time every
// already-stored schedule that does not parse, so the fallback stays and the
// validation goes in front of it.
//
// The same value is returned for an expression that parses but can never match
// (`0 0 30 2 *` -- there is no 30th of February), after a four-year search.
func NextCronTime(cronExpr string, from time.Time) time.Time {
	e, err := parseCronExpr(cronExpr)
	if err != nil {
		return from.Add(24 * time.Hour) // fallback: daily
	}

	// Start at the next whole minute: "next" is strictly after from.
	t := from.Truncate(time.Minute).Add(time.Minute)

	// Search up to 4 years ahead. Four covers a leap year, which is the
	// longest legitimate gap (`0 0 29 2 *` fires once every four years).
	end := from.AddDate(4, 0, 0)
	for t.Before(end) {
		if e.dayMatches(t) && e.hour.matches(t.Hour()) && e.minute.matches(t.Minute()) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return from.Add(24 * time.Hour)
}

// matchField and daysInMonth used to live here, and are deliberately gone.
//
// matchField was the old per-field predicate, re-parsing the pattern text on
// every one of NextCronTime's up-to-two-million search iterations. Parsing once
// into a cronField replaces it. Keeping it as a thin wrapper purely so the
// field-level tests had something to call would have made it test-only code,
// which scripts/check-test-only-code.sh correctly refuses -- and it would have
// meant those tests exercised a shim rather than the code that runs. They call
// parseCronField now.
//
// daysInMonth guarded NextCronTime against matching a day that does not exist
// in the current month (February 30th). It is unreachable now: the search walks
// real time.Time values, so a nonexistent date is never visited at all, and the
// day-of-month field is range-checked once at parse time.
