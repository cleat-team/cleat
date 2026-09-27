// The AssemblyScript fixture for the host-call execution harness: it runs
// exactly ONE host call per invocation and reports what that call did.
//
// Mirrors testdata/hostcallsgo/main.go argument for argument -- the same run
// IDs, the same promise name, the same service and plugin names -- so that a
// difference between two languages' tables is a difference between the SDKs
// and not between the fixtures.
//
// ## What this fixture cannot report, and why that is the finding
//
// Several AssemblyScript bindings spell failure as `null` and carry no error
// text at all: awaitAllChildren, awaitAnyChild, listCrons and sideEffect are
// all `string | null`. When one of them fails, the host's message is gone
// before the guest ever sees it -- so the `detail` for those calls is a string
// this fixture invents, marked "<no host message: ...>", not something the
// host said.
//
// That is the §3.200 shape (a guest that decodes the host's error length and
// throws the message away) except by API design rather than by defect, so
// there is no bug to fix in the fixture and the tables record the loss
// instead. Re-derive the list with:
//
//     grep -nE "^  (awaitAllChildren|awaitAnyChild|listCrons|sideEffect)\(" \
//       packages/cleat-as/assembly/host-calls.ts

import {
  HostCalls,
  cleatEntry,
  deferFunc,
  AwaitPromiseOutcome,
  AwaitSignalsOutcome,
  CancellationStatus,
  CleatCallOutcome,
  ChildWorkflowOptions,
  DurableResult,
  PluginCallOutcome,
  PollSignalOutcome,
  PromiseResult,
} from "@cleat/sdk";

/** Minimal JSON-string escaping, as in testdata/asworkflow. */
function esc(s: string): string {
  let out: string = "";
  for (let i: i32 = 0; i < s.length; i++) {
    let c: string = s.charAt(i);
    if (c == '"') out += '\\"';
    else if (c == "\\") out += "\\\\";
    else if (c == "\n") out += "\\n";
    else if (c == "\r") out += "\\r";
    else if (c == "\t") out += "\\t";
    else out += c;
  }
  return out;
}

/**
 * The outcome object the harness decodes.
 *
 * Built by hand rather than serialised: AssemblyScript has no reflection, and
 * the object is three known string fields.
 */
function emit(call: string, status: string, detail: string): string {
  return (
    '{"call":"' + esc(call) + '","status":"' + status + '","detail":"' + esc(detail) + '"}'
  );
}

function ok(call: string, detail: string): string {
  return emit(call, "ok", detail);
}

function bad(call: string, err: string): string {
  return emit(call, "error", err);
}

/**
 * A failure this SDK reports as `null`, with no host message available.
 *
 * Deliberately NOT the plain error path: the detail below is written by this
 * fixture, so recording it as an ordinary error would put invented text where
 * the Go table puts the host's own words, and the two tables would look
 * comparable when they are not.
 */
function nullErr(call: string, what: string): string {
  return bad(call, "<no host message: " + what + " returns string|null and discards it>");
}

/** deferFunc needs a top-level function; a closure will not do. */
function noopDefer(h: HostCalls, payload: string): void {}

@cleatEntry("HostCalls")
export function exercise_host_call(h: HostCalls, input: string): string {
  // The harness sends {"call":"X"}. Parsing it with the SDK's JSON helpers
  // would put another host call in the middle of every measurement, so the
  // name is extracted directly.
  let key: string = '"call":"';
  let i: i32 = input.indexOf(key);
  if (i < 0) {
    return emit("", "error", "fixture: undecodable input " + input);
  }
  let rest: string = input.substring(i + key.length);
  let j: i32 = rest.indexOf('"');
  let call: string = j < 0 ? rest : rest.substring(0, j);

  // ---- children ----
  if (call == "ChildWorkflow") {
    let r: DurableResult<string> = h.childWorkflow("child-workflow", "{}");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  if (call == "ChildWorkflowWithOptions") {
    let opts: ChildWorkflowOptions = new ChildWorkflowOptions();
    opts.version = 1;
    let r: DurableResult<string> = h.childWorkflowWithOptions("child-workflow", "{}", opts);
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  if (call == "AwaitChild") {
    let r: DurableResult<string> = h.awaitChild("00000000-0000-0000-0000-000000000001");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  if (call == "AwaitAllChildren") {
    let r: string | null = h.awaitAllChildren('["00000000-0000-0000-0000-000000000001"]');
    if (r === null) return nullErr(call, "awaitAllChildren");
    // Reports the host's raw JSON, NOT a count.
    //
    // The first version counted commas to match Go's "N child result(s)", and
    // reported 2 for a one-element array -- the array's single element is an
    // object with a comma inside it. AssemblyScript has no JSON parser in
    // scope here (the SDK's jsonParse is itself a host call, which would put a
    // second call inside every measurement of this one), so there is no honest
    // way to produce the count.
    //
    // A fixture that computes a wrong number and reports it confidently is the
    // exact failure this harness exists to catch, so it reports what it
    // actually received instead. The row is therefore shaped differently from
    // Go's and Rust's, and its `why` says so.
    return ok(call, r);
  }

  if (call == "AwaitAnyChild") {
    let r: string | null = h.awaitAnyChild('["00000000-0000-0000-0000-000000000001"]');
    if (r === null) return nullErr(call, "awaitAnyChild");
    return ok(call, r);
  }

  if (call == "PollChild") {
    let r: DurableResult<string> = h.pollChild("00000000-0000-0000-0000-000000000001");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  // ---- promises ----
  if (call == "CreatePromise") {
    let r: PromiseResult = h.createPromise("harness-promise");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  if (call == "AwaitPromise") {
    // Ms variant: Go passes 10 * time.Millisecond, and awaitPromise() takes
    // SECONDS, so the plain form would round the harness's 10ms to 0 and ask a
    // different question.
    let r: AwaitPromiseOutcome = h.awaitPromiseMs("00000000-0000-0000-0000-000000000002", 10);
    if (r.error !== null) return bad(call, r.error!);
    return ok(call, "timedOut=" + (r.timedOut ? "true" : "false") + " " + r.value);
  }

  // ---- signals ----
  if (call == "DurableAwaitSignals") {
    let r: AwaitSignalsOutcome = h.awaitSignalsMs('["harness-signal"]', 10);
    if (r.error !== null) return bad(call, r.error!);
    return ok(
      call,
      "timedOut=" + (r.timedOut ? "true" : "false") + " " + r.signalName + " " + r.payload,
    );
  }

  if (call == "PollSignal") {
    let r: PollSignalOutcome = h.pollSignal("harness-signal");
    if (r.error !== null) return bad(call, r.error!);
    return ok(call, "present=" + (r.found ? "true" : "false") + " " + r.payload);
  }

  // ---- durable calls ----
  if (call == "DurableCall") {
    let r: CleatCallOutcome = h.cleatCall("harness-service", "harness-op", "{}");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.response);
  }

  if (call == "DurableCallWithHeartbeat") {
    let r: CleatCallOutcome = h.cleatCallHeartbeat("harness-service", "harness-op", "{}", 1000);
    return r.error !== null ? bad(call, r.error!) : ok(call, r.response);
  }

  if (call == "DurableCallWithRetry") {
    // maxAttempts 1, 1ms intervals, coefficient 1.00 -- the same policy the Go
    // and Rust fixtures pass. backoffCoefficient100x is hundredths, so 100.
    let r: CleatCallOutcome = h.cleatCallRetry(
      "harness-service",
      "harness-op",
      "{}",
      1,
      1,
      100,
      1,
      "[]",
    );
    return r.error !== null ? bad(call, r.error!) : ok(call, r.response);
  }

  // ---- defers ----
  if (call == "DurableDefer") {
    let r: DurableResult<string> = h.defer("harness defer");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  if (call == "DurableDeferFunc") {
    // AssemblyScript has no closures that survive the boundary, so deferFunc
    // takes a top-level function plus a payload string. Go's takes a closure.
    // The row records what each proves.
    let r: DurableResult<string> = deferFunc(h, "harness defer", noopDefer, "");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  // ---- crons ----
  if (call == "ScheduleCron") {
    let r: DurableResult<string> = h.scheduleCron("harness-workflow", "0 0 * * *", "UTC", "{}");
    return r.error !== null ? bad(call, r.error!) : ok(call, r.value);
  }

  if (call == "ListCrons") {
    let r: string | null = h.listCrons();
    if (r === null) return nullErr(call, "listCrons");
    return ok(call, r);
  }

  // ---- plugins ----
  if (call == "PluginCall") {
    // Both paths in one row, as in Go: llm.list_models is the only plugin call
    // that works with no tenant, and §3.200 lived entirely on the error path.
    let good: PluginCallOutcome = h.pluginCall("llm", "list_models", "{}");
    if (good.error !== null) return bad(call, good.error!);
    let failing: PluginCallOutcome = h.pluginCall(
      "blobstore",
      "put",
      '{"key":"k","data":"aGk="}',
    );
    if (failing.error === null) {
      return ok(call, "ok=" + good.response + " err=<none: blobstore.put unexpectedly succeeded>");
    }
    return ok(call, "ok=" + good.response + " err=" + failing.error!);
  }

  if (call == "PluginCallStreaming") {
    let r: PluginCallOutcome = h.pluginCallStreaming(
      "llm",
      "chat_stream",
      '{"messages":[{"role":"user","content":"hi"}]}',
    );
    return r.error !== null ? bad(call, r.error!) : ok(call, r.response);
  }

  // ---- locks ----
  if (call == "AcquireLock") {
    // Ms variant, because Go passes 60000 milliseconds and acquireLock() takes
    // seconds. Twice, for the reason the Go fixture gives: a row that only
    // ever sees `true` cannot tell a decoded bit from a hardcoded one.
    let first: DurableResult<bool> = h.acquireLockMs("harness-lock", 60000);
    if (first.error !== null) return bad(call, first.error!);
    let second: DurableResult<bool> = h.acquireLockMs("harness-lock", 60000);
    if (second.error !== null) return bad(call, second.error!);
    return ok(
      call,
      "first=" + (first.value ? "true" : "false") + " second=" + (second.value ? "true" : "false"),
    );
  }

  // ---- identity and control ----
  if (call == "WorkflowID") {
    return ok(call, h.currentWorkflowId());
  }

  if (call == "RunID") {
    return ok(call, h.currentRunId());
  }

  if (call == "PollCancellation") {
    let r: CancellationStatus = h.pollCancellation();
    return ok(call, "cancelled=" + (r.cancelled ? "true" : "false") + " " + r.reason);
  }

  if (call == "SideEffect") {
    // Takes the computed value, like Rust and unlike Go's closure.
    let r: string | null = h.sideEffect("side-effect-value");
    if (r === null) return nullErr(call, "sideEffect");
    return ok(call, r);
  }

  // An unknown name means the fixture and the harness's call list have
  // drifted. Reported as an error outcome rather than a trap because
  // AssemblyScript entries cannot return an error separately from a value --
  // the table has no row for a name it does not expect, so an unknown name
  // still fails, via assertOutcome's "no row in the expected-outcome table".
  return emit(call, "error", "fixture: no case for host call " + call);
}
