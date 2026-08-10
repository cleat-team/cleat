package engine

import (
	"fmt"
	"sort"
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
	// sorted is vals in ascending order. NextCronTimeIn walks candidate wall
	// times in order and returns the first one after `from`, which requires an
	// ordering that a map does not have.
	sorted []int
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

	f.sorted = make([]int, 0, len(f.vals))
	for v := range f.vals {
		f.sorted = append(f.sorted, v)
	}
	sort.Ints(f.sorted)

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

// DefaultScheduleTimezone is the zone a schedule is evaluated in when it does
// not name one. UTC, because a schedule that means "02:00" should keep meaning
// the same instant regardless of which worker in the fleet happens to pick it
// up, and because UTC has no DST transitions to reason about.
const DefaultScheduleTimezone = "UTC"

// ValidateTimezone reports whether name is an IANA timezone this process can
// load, returning nil if it is.
//
// Worth knowing where the answer comes from: time.LoadLocation reads the
// system zoneinfo database, so on a container image without tzdata installed
// every name except "UTC" and "Local" fails. cmd/cleat-worker imports
// _ "time/tzdata" so the worker carries its own copy and does not depend on
// the base image -- see the comment there.
func ValidateTimezone(name string) error {
	if name == "" {
		return nil // caller substitutes DefaultScheduleTimezone
	}
	if _, err := time.LoadLocation(name); err != nil {
		return fmt.Errorf("cron: timezone %q: %w", name, err)
	}
	return nil
}

// LoadScheduleLocation resolves a schedule's timezone name to a *time.Location,
// falling back to UTC when the name is empty or unloadable. The bool reports
// whether the fallback was taken, so a caller can log the difference between
// "this schedule is UTC" and "this schedule wanted a zone this process cannot
// load" -- which are very different operational situations and must not look
// the same in the logs.
func LoadScheduleLocation(name string) (*time.Location, bool) {
	if name == "" || name == DefaultScheduleTimezone {
		return time.UTC, name == ""
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC, true
	}
	return loc, false
}

// scheduleTimezoneOrDefault is what the stores write to workflow_schedules.
//
// The column is NOT NULL, and an empty string would be a third state alongside
// "UTC" and a real zone name -- indistinguishable on read from a schedule
// whose author meant UTC. Normalising on the way in keeps the column able to
// answer "which zone was chosen" without the reader having to know that empty
// means UTC.
func scheduleTimezoneOrDefault(tz string) string {
	if tz == "" {
		return DefaultScheduleTimezone
	}
	return tz
}

// cronInstant returns the absolute instant for the wall-clock time (h:m) on the
// civil date y/mo/d in loc.
//
// This is where daylight saving is decided, and it is not decided by
// time.Date alone.
//
// AMBIGUOUS WALL TIMES (autumn, clocks go back). 01:30 happens twice on
// 2024-11-03 in America/New_York. time.Date returns the FIRST of the two
// (01:30 EDT, 05:30 UTC) -- measured, not assumed. Constructing exactly one
// instant per (date, wall time) is what makes the schedule fire once rather
// than twice, and taking the first is the rule this engine promises.
//
// NONEXISTENT WALL TIMES (spring, clocks go forward). 02:30 does not occur on
// 2024-03-10 in America/New_York: the clock jumps 01:59:59 -> 03:00:00.
// time.Date does NOT normalise such a time forward to 03:30 as one might
// expect -- it normalises BACKWARDS, returning 01:30 EST (06:30 UTC), an hour
// EARLIER than asked for. Taking that value would fire a 02:30 job at 01:30,
// once a year, in the wrong direction. So a nonexistent wall time is detected
// by round-tripping the fields and resolved forward to the transition instant
// (03:00 EDT), which is the promised rule: a skipped wall time fires at the
// next instant that exists, rather than being silently dropped for the day.
func cronInstant(y int, mo time.Month, d, h, m int, loc *time.Location) time.Time {
	t := time.Date(y, mo, d, h, m, 0, 0, loc)
	if t.Hour() == h && t.Minute() == m && t.Day() == d {
		return t
	}

	// The wall time does not exist. Walk forward from where time.Date landed
	// to the first instant whose local clock has reached the requested time.
	// The largest forward transition in tzdata is a small number of hours; the
	// day bound stops this from running away if a zone ever does something
	// stranger.
	for probe := t; probe.Day() == d; probe = probe.Add(time.Minute) {
		if ph, pm := probe.Hour(), probe.Minute(); ph > h || (ph == h && pm >= m) {
			return probe
		}
	}
	return t
}

// NextCronTimeIn computes the next firing time strictly after from, evaluating
// the expression's wall-clock fields in loc.
//
// It walks civil days rather than absolute minutes, because "07:00 every day"
// is a statement about a wall clock and a wall clock is not a fixed offset from
// UTC. A minute-by-minute scan over absolute time gets DST wrong in both
// directions: it fires twice in autumn and not at all in spring.
//
// A nil loc is UTC.
func NextCronTimeIn(expr string, from time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.UTC
	}
	e, err := parseCronExpr(expr)
	if err != nil {
		return from.Add(24 * time.Hour) // fallback: daily. See NextCronTime.
	}

	local := from.In(loc)
	y, mo, d := local.Date()

	// Four years of days, for the same reason NextCronTime searched four
	// years: `0 0 29 2 *` fires once every four.
	const maxDays = 4*366 + 1
	for i := 0; i < maxDays; i++ {
		// Probe the civil date at noon. No zone in tzdata shifts by twelve
		// hours, so noon always exists and always lands on the date asked
		// for. Midnight does not have that property -- zones have historically
		// transitioned at midnight, and constructing one would silently roll
		// the date to the next day.
		probe := time.Date(y, mo, d+i, 12, 0, 0, 0, loc)
		if !e.dayMatches(probe) {
			continue
		}
		py, pmo, pd := probe.Date()
		for _, h := range e.hour.sorted {
			for _, m := range e.minute.sorted {
				if cand := cronInstant(py, pmo, pd, h, m, loc); cand.After(from) {
					return cand
				}
			}
		}
	}
	return from.Add(24 * time.Hour)
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
// It evaluates in from's own location, which for a caller passing time.Now()
// is the host's local zone. That is exactly the ambiguity NextCronTimeIn
// exists to remove: two workers in different zones computed different firing
// times for the same schedule. Prefer NextCronTimeIn with the schedule's
// stored timezone.
func NextCronTime(cronExpr string, from time.Time) time.Time {
	return NextCronTimeIn(cronExpr, from, from.Location())
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

// Schedule policy values. These are stored as text and read by a background
// loop that has nobody to report a bad value to, so the database carries CHECK
// constraints as well -- a value the scheduler cannot interpret must be
// impossible to store rather than handled at 03:00.
const (
	// MisfireCatchUp delivers firings missed during an outage, one instant per
	// poll tick, up to the schedule's catch-up limit. The default, because the
	// engine promises at-least-once.
	MisfireCatchUp = "catch_up"

	// MisfireSkip resumes at the next future instant and delivers none of the
	// backlog. For schedules where a late firing is worse than no firing -- a
	// "send the 09:00 digest" job has nothing useful to say at 14:00.
	MisfireSkip = "skip"

	// OverlapAllow starts a run even if the previous one is still going. The
	// default only because it is what the scheduler has always done: changing
	// it would silently alter existing deployments. It is the wrong default for
	// most real schedules, since a job that occasionally overruns its interval
	// quietly becomes an unbounded fan-out.
	OverlapAllow = "allow"

	// OverlapSkip does not start a run while the previous one from this
	// schedule is still running or ready.
	OverlapSkip = "skip"

	// DefaultCatchUpLimit bounds catch_up when a schedule does not set its own.
	//
	// Generous for the schedules the bound is meant to protect -- hourly and
	// slower cannot reach it within any plausible outage -- and small enough
	// that a catch-up burst stays in the same order of magnitude as a normal
	// minute of work. It is a judgement call, not a measurement.
	DefaultCatchUpLimit = 60
)

// ValidateMisfirePolicy reports whether p is a misfire policy the scheduler
// understands. Empty is valid and means MisfireCatchUp.
func ValidateMisfirePolicy(p string) error {
	switch p {
	case "", MisfireCatchUp, MisfireSkip:
		return nil
	}
	return fmt.Errorf("cron: misfire policy %q: want %q or %q", p, MisfireCatchUp, MisfireSkip)
}

// ValidateOverlapPolicy reports whether p is an overlap policy the scheduler
// understands. Empty is valid and means OverlapAllow.
func ValidateOverlapPolicy(p string) error {
	switch p {
	case "", OverlapAllow, OverlapSkip:
		return nil
	}
	return fmt.Errorf("cron: overlap policy %q: want %q or %q", p, OverlapAllow, OverlapSkip)
}

// MisfirePolicyOrDefault and friends normalise on the way into the store, so
// the column never holds an empty string that a reader has to know means
// something.
func MisfirePolicyOrDefault(p string) string {
	if p == "" {
		return MisfireCatchUp
	}
	return p
}

func OverlapPolicyOrDefault(p string) string {
	if p == "" {
		return OverlapAllow
	}
	return p
}

func CatchUpLimitOrDefault(n int) int {
	if n <= 0 {
		return DefaultCatchUpLimit
	}
	return n
}
