// Package scheduledbackup provides scheduled PostgreSQL backups with pg_dump.
// It supports cron-based scheduling and manual backup via HTTP API and CLI
// commands, and records backup history in PostgreSQL. Dumps are written to
// local disk only -- there is no restore path and no off-host upload.
package scheduledbackup

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// cronField represents a parsed cron field with matching logic.
type cronField struct {
	values  map[int]bool // explicit values
	any     bool         // matches any value
	step    int          // step for */N or range/step patterns
	stepMin int          // implicit minimum for step patterns
}

// cronExpr holds the parsed 5-field cron expression.
type cronExpr struct {
	minute     cronField
	hour       cronField
	dayOfMonth cronField
	month      cronField
	dayOfWeek  cronField
}

// parseCron parses a 5-field cron expression string.
func parseCron(expr string) (*cronExpr, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("cron expression must have 5 fields, got %d", len(fields))
	}

	var ce cronExpr
	var err error

	ce.minute, err = parseField(fields[0], 0, 59)
	if err != nil {
		return nil, fmt.Errorf("minute: %w", err)
	}
	ce.hour, err = parseField(fields[1], 0, 23)
	if err != nil {
		return nil, fmt.Errorf("hour: %w", err)
	}
	ce.dayOfMonth, err = parseField(fields[2], 1, 31)
	if err != nil {
		return nil, fmt.Errorf("day of month: %w", err)
	}
	ce.month, err = parseField(fields[3], 1, 12)
	if err != nil {
		return nil, fmt.Errorf("month: %w", err)
	}
	ce.dayOfWeek, err = parseField(fields[4], 0, 6)
	if err != nil {
		return nil, fmt.Errorf("day of week: %w", err)
	}

	return &ce, nil
}

// parseField parses a single cron field value.
func parseField(field string, min, max int) (cronField, error) {
	cf := cronField{}

	if field == "*" {
		cf.any = true
		return cf, nil
	}

	// Handle */N
	if strings.HasPrefix(field, "*/") {
		step, err := strconv.Atoi(field[2:])
		if err != nil || step <= 0 {
			return cf, fmt.Errorf("invalid step: %q", field)
		}
		cf.any = true
		cf.step = step
		cf.stepMin = min
		return cf, nil
	}

	// Handle comma-separated list
	if strings.Contains(field, ",") {
		cf.values = make(map[int]bool)
		for _, part := range strings.Split(field, ",") {
			v, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil || v < min || v > max {
				return cf, fmt.Errorf("invalid value %q in list", part)
			}
			cf.values[v] = true
		}
		return cf, nil
	}

	// Handle range with optional step: N-M or N-M/K
	if strings.Contains(field, "-") {
		parts := strings.SplitN(field, "-", 2)
		rangeStart, err := strconv.Atoi(parts[0])
		if err != nil || rangeStart < min || rangeStart > max {
			return cf, fmt.Errorf("invalid range start: %q", parts[0])
		}

		rangeEndStr := parts[1]
		step := 1
		if idx := strings.IndexByte(parts[1], '/'); idx >= 0 {
			stepStr := parts[1][idx+1:]
			s, err := strconv.Atoi(stepStr)
			if err != nil || s <= 0 {
				return cf, fmt.Errorf("invalid range step: %q", stepStr)
			}
			step = s
			rangeEndStr = parts[1][:idx]
		}

		rangeEnd, err := strconv.Atoi(rangeEndStr)
		if err != nil || rangeEnd < min || rangeEnd > max || rangeEnd < rangeStart {
			return cf, fmt.Errorf("invalid range end: %q", rangeEndStr)
		}

		cf.values = make(map[int]bool)
		for v := rangeStart; v <= rangeEnd; v += step {
			cf.values[v] = true
		}
		return cf, nil
	}

	// Single value
	v, err := strconv.Atoi(field)
	if err != nil || v < min || v > max {
		return cf, fmt.Errorf("invalid value %q (expected %d-%d)", field, min, max)
	}
	cf.values = map[int]bool{v: true}
	return cf, nil
}

// matches returns true if the given value matches the cron field.
func (f *cronField) matches(val int) bool {
	if f.any && f.step == 0 {
		return true
	}
	if f.any && f.step > 0 {
		return (val-f.stepMin)%f.step == 0
	}
	return f.values[val]
}

// matches returns true if the time matches the cron expression.
func (ce *cronExpr) matches(t time.Time) bool {
	return ce.minute.matches(t.Minute()) &&
		ce.hour.matches(t.Hour()) &&
		ce.dayOfMonth.matches(t.Day()) &&
		ce.month.matches(int(t.Month())) &&
		ce.dayOfWeek.matches(int(t.Weekday()))
}

// nextRun computes the next time matching the cron expression after `from`.
// It iterates minute by minute, up to one year ahead, and returns the zero
// time if no match is found within that window.
func nextRun(cron string, from time.Time) time.Time {
	ce, err := parseCron(cron)
	if err != nil {
		return time.Time{}
	}

	// Start from the next full minute.
	t := from.Truncate(time.Minute).Add(time.Minute)
	deadline := from.AddDate(1, 0, 0) // search up to one year

	for t.Before(deadline) {
		if ce.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}

	return time.Time{}
}
