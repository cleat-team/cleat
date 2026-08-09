package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
// unmarshals into. The duplication is deliberate: engine must not import the
// SDK package, and the wire shape is a contract either way, so it is better
// written down twice and asserted equal in a test than inferred.
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
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeScheduleCron || rec.CronWorkflowName != workflowName || rec.CronExpr != cronExpr {
				return s.cronReplayDivergence(ctx, m, rec,
					fmt.Sprintf("ScheduleCron mismatch.\n  workflow: %s %q\n  history: %s %q",
						workflowName, cronExpr, rec.CronWorkflowName, rec.CronExpr),
					idPtr, idMaxLen)
			}
			if rec.Err != "" {
				written, _ := s.writeResult(ctx, m, idPtr, rec.Err, idMaxLen)
				return packSimpleResult(1, written)
			}
			written, _ := s.writeResult(ctx, m, idPtr, rec.CronScheduleID, idMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	scheduleID, err := s.createCronSchedule(ctx, workflowName, cronExpr, timezone, inputJSON)

	rec := EventRecord{
		Step:             s.stepCount,
		EventType:        EventTypeScheduleCron,
		CronWorkflowName: workflowName,
		CronExpr:         cronExpr,
		CronTimezone:     timezone,
		CronInput:        inputJSON,
		CronScheduleID:   scheduleID,
	}
	if err != nil {
		rec.Err = err.Error()
	}
	s.recordEvent(rec)

	if err != nil {
		s.engine.log().ErrorContext(ctx, "schedule_cron failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		written, _ := s.writeResult(ctx, m, idPtr, err.Error(), idMaxLen)
		return packSimpleResult(1, written)
	}
	written, _ := s.writeResult(ctx, m, idPtr, scheduleID, idMaxLen)
	return packSimpleResult(0, written)
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
		Enabled:        true,
		NextRunAt:      NextCronTimeIn(cronExpr, time.Now(), loc),
		Timezone:       tz,
	}); err != nil {
		return "", fmt.Errorf("schedule_cron %q: %w", workflowName, err)
	}
	return scheduleID, nil
}

// DeleteCron removes a recurring trigger by schedule ID.
func (s *execSession) DeleteCron(ctx context.Context, m api.Module, scheduleID string) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeDeleteCron || rec.CronScheduleID != scheduleID {
				if s.engine.Metrics != nil {
					s.engine.Metrics.RecordReplayFailure(ctx)
				}
				return packSimpleResult(1)
			}
			if rec.Err != "" {
				return packSimpleResult(1)
			}
			return packSimpleResult(0)
		}
		s.exitReplay()
	}

	var err error
	if store := s.engine.workflowStore; store != nil {
		// Deleting a schedule that is not there is the success a retry should
		// see, and the stores already report no error for zero rows.
		err = store.DeleteSchedule(ctx, scheduleID)
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
	s.recordEvent(rec)

	if err != nil {
		s.engine.log().ErrorContext(ctx, "delete_cron failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		return packSimpleResult(1)
	}
	return packSimpleResult(0)
}

// ListCrons returns the tenant's schedules as a JSON array.
//
// The result is journaled like any other observation of outside state: the
// set of schedules changes over time, so a replay that re-read it would see a
// different answer than the run recorded.
func (s *execSession) ListCrons(ctx context.Context, m api.Module, outPtr, outMaxLen uint32) int64 {
	if s.isReplay {
		if s.stepCount < len(s.history) {
			rec := s.history[s.stepCount]
			if !s.advanceReplayStep(ctx, &rec) {
				return 0
			}
			if rec.EventType != EventTypeListCrons {
				return s.cronReplayDivergence(ctx, m, rec,
					fmt.Sprintf("ListCrons mismatch.\n  history has: %s", rec.EventType),
					outPtr, outMaxLen)
			}
			if rec.Err != "" {
				written, _ := s.writeResult(ctx, m, outPtr, rec.Err, outMaxLen)
				return packSimpleResult(1, written)
			}
			written, _ := s.writeResult(ctx, m, outPtr, rec.CronResult, outMaxLen)
			return packSimpleResult(0, written)
		}
		s.exitReplay()
	}

	listJSON, err := s.listCronSchedules(ctx)

	rec := EventRecord{
		Step:       s.stepCount,
		EventType:  EventTypeListCrons,
		CronResult: listJSON,
	}
	if err != nil {
		rec.Err = err.Error()
	}
	s.recordEvent(rec)

	if err != nil {
		s.engine.log().ErrorContext(ctx, "list_crons failed", "workflow_id", s.workflowID, "tenant_id", s.tenantID, "error", err)
		written, _ := s.writeResult(ctx, m, outPtr, err.Error(), outMaxLen)
		return packSimpleResult(1, written)
	}
	written, _ := s.writeResult(ctx, m, outPtr, listJSON, outMaxLen)
	return packSimpleResult(0, written)
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
			Enabled:      schedules[i].Enabled,
		})
	}
	out, err := json.Marshal(views)
	if err != nil {
		return "", fmt.Errorf("list_crons: %w", err)
	}
	return string(out), nil
}

// cronReplayDivergence reports a history entry that does not match the call
// being replayed, in the same shape the other host calls use.
func (s *execSession) cronReplayDivergence(ctx context.Context, m api.Module, rec EventRecord, detail string, outPtr, outMaxLen uint32) int64 {
	if s.engine.Metrics != nil {
		s.engine.Metrics.RecordReplayFailure(ctx)
	}
	msg := fmt.Sprintf("replay divergence at step %d: %s\nRun 'cleat vet' on your workflow code to check for common non-determinism issues (time.Now(), random values, map iteration, goroutines).",
		rec.Step, detail)
	written, _ := s.writeResult(ctx, m, outPtr, msg, outMaxLen)
	return packSimpleResult(1, written)
}
