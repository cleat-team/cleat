package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// Cron host calls: a workflow can register, remove, and list recurring
// triggers for other workflows.
//
// These are the guest-facing entry point into the schedule subsystem that
// until now only the CLI and the admin API could reach. Everything a
// scheduled firing promises -- at-least-once delivery, a timezone the cron
// fields are evaluated in, a misfire policy -- is already enforced by the
// worker's scheduler loop and the stores; this file only adds the door.

// cronScheduleView is the JSON shape ListCrons hands the guest. It mirrors
// cleat.CronSchedule field for field, which is the SDK type the guest
// unmarshals into. The duplication is forced: the SDK is a separate module
// that imports this one, so engine cannot import it back.
//
// Nothing in the compiler connects the two, so both sides are pinned to the
// same literal instead: TestCronScheduleViewWireShape here, and
// TestCronScheduleWireShapeMatchesTheEngine in the SDK. Renaming a field on
// either side without the other fails one of them; without those two, it
// would silently break every Go guest that calls ListCrons.
type cronScheduleView struct {
	ScheduleID   string `json:"schedule_id"`
	WorkflowName string `json:"workflow_name"`
	CronExpr     string `json:"cron_expr"`
	Timezone     string `json:"timezone"`
	Input        string `json:"input"`
	Enabled      bool   `json:"enabled"`
}

// scheduleIDFor derives a schedule's name from the call site that created it
// rather than from a fresh random value.
//
// This is what makes ScheduleCron safe to retry. A workflow that creates a
// schedule and then crashes before its event reaches the journal will replay
// and reach this same call at this same step, and a random name would leave
// the first schedule behind with nobody holding its ID -- firing forever,
// unreferenced and undeletable through the API that created it. A derived
// name means the retry addresses the same row, so the create can be made
// idempotent and the orphan cannot exist.
//
// The hash keeps the result inside readServiceName's charset no matter what
// the workflow ID contains.
func scheduleIDFor(tenantID, workflowID string, step int) string {
	h := sha256.Sum256([]byte(tenantID + "\x00" + workflowID + "\x00" + strconv.Itoa(step)))
	return "cron-" + hex.EncodeToString(h[:10])
}

// ScheduleCron registers a recurring trigger and returns its schedule ID.
func (s *execSession) ScheduleCron(ctx context.Context, m api.Module, workflowName, cronExpr, timezone, inputJSON string, idPtr, idMaxLen uint32) int64 {
	return s.step(ctx, m, &scheduleCronStep, stepArgs{s1: workflowName, s2: cronExpr, s3: timezone, s4: inputJSON, outPtr: idPtr, outMaxLen: idMaxLen})
}

// scheduleCronStep is ScheduleCron on the dispatcher (cleat#4152, G1c).
//
// A fresh schedule_cron is new work with the longest reach of any call in this family: it registers a RECURRING trigger, so a terminated
// workflow would keep starting new runs on a cron schedule indefinitely. The dispatcher refuses it where the call did.
//
// IDENTITY is all four arguments, not just the two that name the schedule, and that is today's check carried over unchanged (the design's
// table says "the cron name"; this is stricter, and loosening it would be a behaviour change). A run that changed only the timezone would
// otherwise be handed the recorded schedule's ID and carry on believing it had scheduled in the new zone, which is precisely the class of
// thing the check exists to catch.
var scheduleCronStep = stepSpec{
	Name:    "schedule_cron",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypeScheduleCron,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.CronWorkflowName != a.s1 || rec.CronExpr != a.s2 || rec.CronTimezone != a.s3 || rec.CronInput != a.s4 {
			return fmt.Sprintf("schedule %q %q tz=%q input=%s, recorded %q %q tz=%q input=%s",
				a.s1, a.s2, a.s3, truncateWithHash(a.s4, maxPayloadLen),
				rec.CronWorkflowName, rec.CronExpr, rec.CronTimezone, truncateWithHash(rec.CronInput, maxPayloadLen))
		}
		return ""
	},
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		if rec.Err != "" {
			written, _ := c.s.writeResult(c.ctx, c.m, c.args.outPtr, rec.Err, c.args.outMaxLen)
			return packSimpleResult(1, written)
		}
		written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, rec.CronScheduleID, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx := c.s, c.ctx
		scheduleID, err := s.createCronSchedule(ctx, c.args.s1, c.args.s2, c.args.s3, c.args.s4)

		rec := EventRecord{
			Step:             s.stepCount,
			EventType:        EventTypeScheduleCron,
			CronWorkflowName: c.args.s1,
			CronExpr:         c.args.s2,
			CronTimezone:     c.args.s3,
			CronInput:        c.args.s4,
			CronScheduleID:   scheduleID,
		}
		if err != nil {
			rec.Err = err.Error()
		}
		c.Record(rec)

		if err != nil {
			s.engine.log().ErrorContext(ctx, "schedule_cron failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			written, _ := s.writeResult(ctx, c.m, c.args.outPtr, err.Error(), c.args.outMaxLen)
			return packSimpleResult(1, written)
		}
		written, writtenEC := s.writeOut(ctx, c.m, c.args.outPtr, scheduleID, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
}

// createCronSchedule does the live half of ScheduleCron: validate, enforce the
// quota, and create the row idempotently.
func (s *execSession) createCronSchedule(ctx context.Context, workflowName, cronExpr, timezone, inputJSON string) (string, error) {
	// Argument validation comes before anything about the host's own
	// configuration: a bad cron expression is the guest's mistake either way,
	// and it should be told which one it made.
	if workflowName == "" {
		return "", fmt.Errorf("schedule_cron: workflow name is empty")
	}
	if err := ValidateCronExpr(cronExpr); err != nil {
		return "", fmt.Errorf("schedule_cron %q: %w", workflowName, err)
	}
	tz := scheduleTimezoneOrDefault(timezone)
	if err := ValidateTimezone(tz); err != nil {
		return "", fmt.Errorf("schedule_cron %q: %w", workflowName, err)
	}
	// The second return is "fell back to UTC", not "ok". tz is non-empty here
	// and ValidateTimezone just loaded it, so a fallback at this point means
	// the zoneinfo database disagrees with itself rather than that the guest
	// passed something bad -- and silently scheduling in UTC instead of the
	// zone the guest asked for would be the wrong way to handle it.
	loc, fellBack := LoadScheduleLocation(tz)
	if fellBack {
		return "", fmt.Errorf("schedule_cron %q: timezone %q could not be loaded", workflowName, tz)
	}
	if inputJSON == "" {
		inputJSON = "{}"
	}
	if !json.Valid([]byte(inputJSON)) {
		return "", fmt.Errorf("schedule_cron %q: input is not valid JSON", workflowName)
	}

	store := s.engine.workflowStore
	if store == nil {
		return "", fmt.Errorf("no workflow store configured: workflow %s cannot schedule %q", s.workflowID, workflowName)
	}

	scheduleID := scheduleIDFor(s.tenantID, s.workflowID, s.stepCount)

	// One read answers both questions below, and bounds itself: the quota is
	// the reason this list cannot grow without limit.
	existing, err := store.ListSchedules(ctx)
	if err != nil {
		return "", fmt.Errorf("schedule_cron %q: list schedules: %w", workflowName, err)
	}
	for i := range existing {
		if existing[i].Name == scheduleID {
			// An earlier attempt at this same step already created it. Report
			// the success that actually happened rather than a duplicate-key
			// error for our own row.
			return scheduleID, nil
		}
	}
	if s.engine.maxQuotaSchedules > 0 && len(existing) >= s.engine.maxQuotaSchedules {
		return "", fmt.Errorf("schedule_cron %q: schedule quota exceeded (current %d, max %d)",
			workflowName, len(existing), s.engine.maxQuotaSchedules)
	}

	// The stores write their own tenant ID; a guest cannot create a schedule
	// outside the tenant its workflow runs in.
	if err := store.CreateSchedule(ctx, Schedule{
		Name:           scheduleID,
		DefName:        workflowName,
		CronExpression: cronExpr,
		Input:          json.RawMessage(inputJSON),
		// No retirement field: after cleat#1702 a LIVE schedule is the zero
		// value (DisabledAt nil), which is the one upside of the polarity
		// change -- `Enabled: true` used to be mandatory here, and forgetting
		// it created a schedule that never fired.
		NextRunAt: NextCronTimeIn(cronExpr, time.Now(), loc),
		Timezone:  tz,
	}); err != nil {
		return "", fmt.Errorf("schedule_cron %q: %w", workflowName, err)
	}
	return scheduleID, nil
}

// DeleteCron removes a recurring trigger by schedule ID.
func (s *execSession) DeleteCron(ctx context.Context, m api.Module, scheduleID string) int64 {
	return s.step(ctx, m, &deleteCronStep, stepArgs{s1: scheduleID})
}

// deleteCronStep is DeleteCron on the dispatcher (cleat#4152, G1c).
//
// A fresh delete_cron is work the workflow BODY is doing past the end of its recorded history. In a defer segment that is a terminated
// workflow's body running on, and it would delete a schedule the workflow never reached, measured against a real store in cleat#4083,
// where it removed the row. The dispatcher refuses it where the call did. A defer BODY still gets through: inDeferDrain is how
// stopBeforeNewWork tells cleanup that deletes its own workflow's schedule from the body.
//
// IDENTITY is the schedule id, as before. A divergence still carries no MESSAGE to the guest, and that is not a choice made here: the ABI
// gives delete_cron no output buffer (outMaxLen 0), and the dispatcher's reporter writes a message only when there is one. It is counted
// (the replay-failure metric) and answers error code 1, as it did.
var deleteCronStep = stepSpec{
	Name:    "delete_cron",
	Effect:  effectEffect,
	Records: recordsOne,
	Want:    EventTypeDeleteCron,
	Identity: func(a stepArgs, rec *EventRecord) string {
		if rec.CronScheduleID != a.s1 {
			return fmt.Sprintf("delete schedule %q, recorded %q", a.s1, rec.CronScheduleID)
		}
		return ""
	},
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		if rec.Err != "" {
			return packSimpleResult(1)
		}
		return packSimpleResult(0)
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx, scheduleID := c.s, c.ctx, c.args.s1
		var err error
		if store := s.engine.workflowStore; store != nil {
			// Deleting a schedule that is not there is the success a retry should
			// see: workflow execution is at-least-once, so a replayed delete of a
			// schedule the first attempt already removed must not fail the
			// workflow.
			//
			// This used to hold by accident. The stores discarded the statement
			// result, so zero rows matched was indistinguishable from one and
			// every delete reported success -- including a delete of a name that
			// never existed, which is what cleat#1297 fixed for the HTTP API.
			// Now that the store reports it, the idempotence is DELIBERATE and
			// stated here, which is where it belongs: the store's job is to say
			// what happened, and the caller's job is to decide what it means.
			// The same not-found that is a 404 to an operator who mistyped a name
			// is a success to a retry that already did the work.
			err = store.DeleteSchedule(ctx, scheduleID)
			if errors.Is(err, ErrScheduleNotFound) {
				err = nil
			}
		} else {
			err = fmt.Errorf("no workflow store configured: workflow %s cannot delete schedule %q", s.workflowID, scheduleID)
		}

		rec := EventRecord{
			Step:           s.stepCount,
			EventType:      EventTypeDeleteCron,
			CronScheduleID: scheduleID,
		}
		if err != nil {
			rec.Err = err.Error()
		}
		c.Record(rec)

		if err != nil {
			s.engine.log().ErrorContext(ctx, "delete_cron failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			return packSimpleResult(1)
		}
		return packSimpleResult(0)
	},
}

// ListCrons returns the tenant's schedules as a JSON array.
//
// The result is journaled like any other observation of outside state: the
// set of schedules changes over time, so a replay that re-read it would see a
// different answer than the run recorded.
func (s *execSession) ListCrons(ctx context.Context, m api.Module, outPtr, outMaxLen uint32) int64 {
	return s.step(ctx, m, &listCronsStep, stepArgs{outPtr: outPtr, outMaxLen: outMaxLen})
}

// listCronsStep is ListCrons on the dispatcher (cleat#4152, G1c).
//
// IT IS NEVER REFUSED (Refuse: neverRefuse), which is what the call did before it moved and not what the design's section 5 would have it do.
// The design's rule is "refuse every non-Pure step at the frontier of a segment's body, unless the harness shows a case that needs one", and
// names ListCrons among the calls it refuses. Today it lists, records the answer and reaches the store in all four states stopBeforeNewWork
// covers (a defer segment's body, at its frontier, after an event write was refused, and after a worker shutdown), measured in the golden
// table (a_cron_single_steps_behave_as_recorded_before_the_dispatcher_test.go, rows segment_*, write_failed_* and shutdown_fresh) and not
// argued. Whether it SHOULD be refused is a separate decision with a cost: a refusal makes it a host stop site that every SDK must check for
// bit 31 (cleat#4239), and nothing here shows a workflow that needs it refused. neverRefuse also keeps the stop-site scans from counting it.
//
// The dispatcher has no Read effect class yet (see stepEffect), so it is declared effectEffect, which only matters for the refusal this
// overrides.
var listCronsStep = stepSpec{
	Name:       "list_crons",
	Effect:     effectEffect,
	Refuse:     neverRefuse,
	Records:    recordsOne,
	Want:       EventTypeListCrons,
	NoIdentity: "the result is the event: a list of the tenant's schedules at that moment, which differs between a run and its replay by construction, and the call has no argument that could identify it",
	Decode: func(c *stepCtx, rec *EventRecord) int64 {
		if rec.Err != "" {
			written, _ := c.s.writeResult(c.ctx, c.m, c.args.outPtr, rec.Err, c.args.outMaxLen)
			return packSimpleResult(1, written)
		}
		written, writtenEC := c.s.writeOut(c.ctx, c.m, c.args.outPtr, rec.CronResult, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
	Fresh: func(c *stepCtx) int64 {
		s, ctx := c.s, c.ctx
		listJSON, err := s.listCronSchedules(ctx)

		rec := EventRecord{
			Step:       s.stepCount,
			EventType:  EventTypeListCrons,
			CronResult: listJSON,
		}
		if err != nil {
			rec.Err = err.Error()
		}
		c.Record(rec)

		if err != nil {
			s.engine.log().ErrorContext(ctx, "list_crons failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
			written, _ := s.writeResult(ctx, c.m, c.args.outPtr, err.Error(), c.args.outMaxLen)
			return packSimpleResult(1, written)
		}
		written, writtenEC := s.writeOut(ctx, c.m, c.args.outPtr, listJSON, c.args.outMaxLen)
		return packSimpleResult(writtenEC, written)
	},
}

func (s *execSession) listCronSchedules(ctx context.Context) (string, error) {
	store := s.engine.workflowStore
	if store == nil {
		return "", fmt.Errorf("no workflow store configured: workflow %s cannot list schedules", s.workflowID)
	}
	schedules, err := store.ListSchedules(ctx)
	if err != nil {
		return "", fmt.Errorf("list_crons: %w", err)
	}
	// ListSchedules orders by name in every backend, so the array a guest
	// sees does not depend on which backend it is running against.
	views := make([]cronScheduleView, 0, len(schedules))
	for i := range schedules {
		views = append(views, cronScheduleView{
			ScheduleID:   schedules[i].Name,
			WorkflowName: schedules[i].DefName,
			CronExpr:     schedules[i].CronExpression,
			Timezone:     scheduleTimezoneOrDefault(schedules[i].Timezone),
			Input:        string(schedules[i].Input),
			// Still `enabled`, and still inverted from the column: this view
			// is the guest-visible shape of list_crons, versioned with the
			// WASM ABI rather than with the entity contract. cleat#1702
			// converted workflow_schedules.enabled to disabled_at; an SDK
			// compiled before that change still reads this field.
			Enabled: !schedules[i].Disabled(),
		})
	}
	out, err := json.Marshal(views)
	if err != nil {
		return "", fmt.Errorf("list_crons: %w", err)
	}
	return string(out), nil
}
