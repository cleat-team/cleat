"""Every method on cleat_sdk.HostCalls, in one workflow, for coverage.

This exists to be COMPILED by componentize-py, not to be run. Its job is to
make the whole Python host-call surface reach the WIT bindings and the
component build, so a binding that does not exist -- or does not accept the
arguments the SDK passes it -- fails the build instead of failing a user.

Why it is needed: the Python end-to-end test compiled
`durable_call_workflow.py`, which calls one host method. Passing it meant
"Python can make a durable call", not "the Python host-call surface builds".
The Go side had the same hole and four host calls shipped uncompilable through
it -- see IMPROVEMENT-PLAN.md 3.204.

The calls live in `_exercise_every_host_call`, which the entry point only
reaches when asked. Several of them (continue_as_new, extend_timeout,
release_lock) would change a running workflow's fate, and this file is on the
compile path, not the behaviour path.

When adding a host call, add it here. `test_all_host_calls_fixture.py` fails
if a public HostCalls method is missing from this file.
"""

from dataclasses import dataclass

from cleat_sdk import ChildWorkflowOptions, HostCalls, RetryPolicy, cleat_entry


@dataclass
class Request:
    exercise: bool = False


@dataclass
class Reply:
    value: str = ""


def _exercise_every_host_call(h: HostCalls) -> None:
    # ---- identity, determinism sources ----
    h.set_scope("obj", "key")
    h.get_scope()
    h.clear_scope()
    h.uuid("seed")
    h.now()
    h.random()
    h.current_workflow_id()
    h.current_run_id()
    h.version()
    h.min_version()

    # ---- logging ----
    h.log("m")
    h.log_kv("m", "k", "v")

    # ---- durable calls ----
    h.call("svc", "op", {"a": 1})
    h.call_typed("svc", "op", {"a": 1}, Reply)
    h.call_with_retry("svc", "op", {"a": 1}, RetryPolicy())
    h.call_with_heartbeat("svc", "op", {"a": 1}, 1000, lambda progress: None)
    h.send("svc", "op", {"a": 1})
    h.schedule_invoke("svc", "op", {"a": 1}, 1000)

    # ---- sleep ----
    h.sleep(1)
    h.sleep_ms(1000)

    # ---- http ----
    h.fetch("http://x")
    h.fetch_json("http://x")
    h.fetch_get("http://x")
    h.fetch_get_json("http://x")
    h.host_fetch("GET", "http://x")

    # ---- signals ----
    h.await_signals(["s"], 1)
    h.await_signals_ms(["s"], 1000)
    h.await_signals_with_quorum(["s"], 1, 0, 1000)
    h.poll_signal("s")
    h.poll_cancellation()
    h.signal_workflow("run-1", "s", {"a": 1})
    h.send_signal_and_wait("run-1", "s", {"a": 1}, 1)
    h.send_signal_and_wait_ms("run-1", "s", {"a": 1}, 1000)
    h.reply_to_signal("corr", {"a": 1})

    # ---- children ----
    h.child_workflow("child", {"a": 1})
    h.child_workflow_with_options("child", {"a": 1}, ChildWorkflowOptions())
    h.await_child("run-1")
    h.await_all_children(["run-1"])

    # ---- state ----
    h.set_query_state("k", "v")
    h.set_state("k", "v")
    h.get_state("k", Reply)
    h.delete_state("k")
    h.incr_state("k", 1)
    h.has_state("k")
    h.list_state("pre")

    # ---- streaming state ----
    h.stream_set_state("k", "v")
    h.stream_get_state("k")
    h.stream_delete_state("k")
    h.stream_incr_state("k", 1)
    h.stream_has_state("k")
    h.stream_list_state("pre")

    # ---- promises ----
    h.create_promise("p")
    h.await_promise("prom-1", 1)
    h.await_promise_ms("prom-1", 1000)
    h.resolve_promise("prom-1", "v")
    h.reject_promise("prom-1", "e")

    # ---- updates, defer, detached ----
    h.register_update_handler("upd", lambda payload: payload)
    h.defer("cleanup")
    h.defer_func(lambda: None)
    h.run_detached(lambda: None)

    # ---- lifecycle ----
    h.continue_as_new({"a": 1})
    h.continue_as_new_versioned({"a": 1}, 2)
    h.extend_timeout(1000)
    h.side_effect("computed")

    # ---- plugins ----
    h.plugin_call("p", "f", {"a": 1})
    h.plugin_call_streaming("p", "f", {"a": 1})
    h.plugin_call_typed("p", "f", {"a": 1}, Reply)

    # ---- crons ----
    h.schedule_cron("wf", "* * * * *", "UTC", "{}")
    h.delete_cron("sched-1")
    h.list_crons()

    # ---- locks ----
    h.acquire_lock("k", 1)
    h.acquire_lock_ms("k", 1000)
    h.release_lock("k")


@cleat_entry
def all_host_calls_workflow(h: HostCalls, request: Request) -> str:
    """Compiles every host call; runs none of them unless asked."""
    if request.exercise:
        _exercise_every_host_call(h)
    return "ok"
