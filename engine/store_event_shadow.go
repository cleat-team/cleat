package engine

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

// event_history stores each event twice: once in the individual columns
// (service, operation, request, ...) and again in a payload JSONB.
// LoadEventHistory scans the columns first and then overwrites them from
// payload whenever it is non-NULL, so payload is authoritative -- and
// computeEventChecksum covers only payload.
//
// That leaves a gap. `UPDATE event_history SET operation = 'something-else'`
// changes nothing the checksum sees: VerifyWorkflowEvents reports the workflow
// clean and replay is unaffected, because both read payload. But every SQL
// consumer that reads the columns -- the admin dashboard, cleatctl, ad-hoc
// queries, metrics -- shows the altered value. An integrity checker that
// certifies a row whose displayed contents are a lie is doing half the job.
//
// The fix here is the cheap one of the three in IMPROVEMENT-PLAN 2.32:
// verification compares the columns against payload. It needs no migration and
// does not touch the chain, so every checksum already stored stays valid. The
// other two options -- extending the checksum to cover the columns, or
// dropping the duplicate columns entirely -- both invalidate stored checksums,
// and the second is the real fix and the largest.
//
// # What this deliberately does not cover
//
// Only fields that are neither encrypted nor redacted. Request, Response, Err,
// SignalPayload, ChildInput, NewInput, PluginInput, PluginOutput,
// PromiseResult and PromiseError all pass through decryptAndRedactEventRecord
// and RedactOnRead on the way out of the column path, while the payload path
// is decrypted but never redacted. Comparing those two would report a
// divergence for every redacted field in the database.
//
// It also only detects divergence on keys the payload actually carries.
// eventRecordToPayload omits several of these when they are empty (duration_ms
// on a call, for instance), and populateFromPayload cannot overwrite a key that
// is not there -- so tampering with a column whose payload counterpart was
// omitted is still invisible. The headline case from 2.32, `operation` on a
// call event, is always present and is detected.

// shadowField is a field mirrored in both an individual column and the payload
// JSONB, and touched by neither encryption nor redaction.
type shadowField struct {
	column string
	get    func(*EventRecord) string
}

var shadowFields = []shadowField{
	{"service", func(r *EventRecord) string { return r.Service }},
	{"operation", func(r *EventRecord) string { return r.Op }},
	{"duration_ms", func(r *EventRecord) string { return strconv.FormatInt(r.DurationMs, 10) }},
	{"signal_names", func(r *EventRecord) string { return r.SignalNames }},
	{"timeout_ms", func(r *EventRecord) string { return strconv.FormatInt(r.TimeoutMs, 10) }},
	{"signal_name", func(r *EventRecord) string { return r.SignalName }},
	{"defer_description", func(r *EventRecord) string { return r.DeferDescription }},
	{"defer_id", func(r *EventRecord) string { return r.DeferID }},
	{"child_name", func(r *EventRecord) string { return r.ChildName }},
	{"run_id", func(r *EventRecord) string { return r.RunID }},
	{"plugin_name", func(r *EventRecord) string { return r.PluginName }},
	{"plugin_func", func(r *EventRecord) string { return r.PluginFunc }},
	{"promise_name", func(r *EventRecord) string { return r.PromiseName }},
	{"promise_id", func(r *EventRecord) string { return r.PromiseID }},
}

// verifyShadowColumns reports the first event whose mirrored columns disagree
// with its payload.
func (s *PostgresStore) verifyShadowColumns(ctx context.Context, tx *sql.Tx, workflowID string) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT step, event_type, service, operation, duration_ms, signal_names,
		       timeout_ms, signal_name, defer_description, defer_id, child_name,
		       run_id, plugin_name, plugin_func, promise_name, promise_id, payload
		FROM event_history
		WHERE workflow_id = $1 AND tenant_id = $2
		ORDER BY step
	`, workflowID, s.tenantID)
	if err != nil {
		// Consistent with the checksum arm above: a schema without these
		// columns is pre-migration, not corrupt.
		return nil
	}
	defer rows.Close()

	for rows.Next() {
		var col EventRecord
		var service, op, signalNames, signalName sql.NullString
		var deferDesc, deferID, childName, runID sql.NullString
		var pluginName, pluginFunc, promiseName, promiseID sql.NullString
		var durationMs, timeoutMs sql.NullInt64
		var payload sql.NullString

		if err := rows.Scan(&col.Step, &col.EventType, &service, &op, &durationMs,
			&signalNames, &timeoutMs, &signalName, &deferDesc, &deferID, &childName,
			&runID, &pluginName, &pluginFunc, &promiseName, &promiseID, &payload); err != nil {
			return fmt.Errorf("verify events: scan shadow columns: %w", err)
		}
		if !payload.Valid {
			// Nothing to compare against: this row predates the payload
			// column, and the checksum arm skips it for the same reason.
			continue
		}

		col.Service = service.String
		col.Op = op.String
		col.DurationMs = durationMs.Int64
		col.SignalNames = signalNames.String
		col.TimeoutMs = timeoutMs.Int64
		col.SignalName = signalName.String
		col.DeferDescription = deferDesc.String
		col.DeferID = deferID.String
		col.ChildName = childName.String
		col.RunID = runID.String
		col.PluginName = pluginName.String
		col.PluginFunc = pluginFunc.String
		col.PromiseName = promiseName.String
		col.PromiseID = promiseID.String

		// populateFromPayload only assigns the keys the payload carries, so
		// the two records can differ only where payload disagrees with the
		// column. Anything the payload omits stays equal by construction --
		// which is why this needs no per-event-type knowledge here.
		fromPayload := col
		populateFromPayload(&fromPayload, []byte(s.decryptPayloadJSON(payload.String)))

		for _, f := range shadowFields {
			stored, authoritative := f.get(&col), f.get(&fromPayload)
			if stored != authoritative {
				return fmt.Errorf("verify events: workflow %s step %d: column %s = %q but payload says %q "+
					"(payload is authoritative for replay, so this row replays correctly while every SQL "+
					"consumer that reads the columns shows the altered value)",
					workflowID, col.Step, f.column, stored, authoritative)
			}
		}
	}
	return rows.Err()
}
