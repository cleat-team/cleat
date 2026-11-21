-- cleat#4285: in-flight workflows whose history the replay now REFUSES.
--
-- A history written by a guest that ignored a child-await suspension holds a pending await (an await_child, await_all_children or await_any_child record with an
-- EMPTY response) followed by a call the guest's body made after it. Before 0.6.0 replay ran that call a second time; from 0.6.0 replay stops at the pending await with
-- a divergence ("... ignored a child-await suspension before 0.6.0 (cleat#4285) ... Terminate or reset the workflow"). This lists the workflows that will hit it, so they
-- can be terminated or reset BEFORE the upgrade. It reads the same three things the replay rule reads and nothing else:
--
--   1. a pending await:  event_type in the three child awaits and an empty response;
--   2. a LATER record that is a call's effect: the event types the engine itself calls stoppable (engine/stop_at_last_stoppable.go, stoppableFirstEvents;
--      promise_resolved / promise_rejected / signal_received are stoppable only by service and op and are left out, which is the narrower and safer reading);
--   3. that record carries NEITHER drain stamp (payload in_defer_phase / in_defer_segment): a defer drain's own records follow its pending await by design.
--
-- WHAT IT CANNOT SEE, and the rule treats the same way: events a compaction folded away are no longer rows here (their stamps are unknown to the replay too, so it
-- does not refuse them). WHAT IT MAY OVER-REPORT: a drain written before cleat#1155 carries no stamp, so its records read as the body's here.
--
-- RUN IT AS A ROLE THAT BYPASSES ROW LEVEL SECURITY (event_history is FORCE ROW LEVEL SECURITY): as the table owner without BYPASSRLS it returns no rows, which reads
-- exactly like "nothing is affected". Check that first with: SELECT count(*) FROM event_history; (0 on a busy system means the role is being filtered).
-- PostgreSQL only. The MySQL and SQL Server equivalents were not written or tested, and --encrypt-sensitive-payloads was not exercised against this query.

SELECT p.workflow_id,
       p.step       AS pending_step,
       p.event_type AS pending_event,
       l.step       AS later_step,
       l.event_type AS later_event
FROM event_history p
JOIN event_history l
  ON l.workflow_id = p.workflow_id
 AND l.tenant_id   = p.tenant_id
 AND l.step        > p.step
JOIN workflow_leases w
  ON w.id = p.workflow_id
 AND w.tenant_id = p.tenant_id
WHERE p.event_type IN ('await_child', 'await_all_children', 'await_any_child')
  AND coalesce(p.response, '') = ''
  AND l.event_type IN ('call', 'await_signals', 'child_workflow', 'plugin_call', 'run_detached', 'side_effect', 'durable_send', 'defer',
                       'fetch', 'acquire_lock', 'schedule_cron', 'durable_schedule_invoke')
  AND coalesce(l.payload ->> 'in_defer_phase', 'false')   <> 'true'
  AND coalesce(l.payload ->> 'in_defer_segment', 'false') <> 'true'
  AND w.status NOT IN ('done', 'failed', 'dead_lettered', 'terminated', 'cancelled')
ORDER BY p.workflow_id, p.step, l.step;
