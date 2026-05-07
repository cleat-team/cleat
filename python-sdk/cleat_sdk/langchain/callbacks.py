"""CleatCallbackHandler: Records LangChain steps as cleat durable events.

Usage::

    from cleat_sdk import HostCalls, cleat_entry
    from cleat_sdk.langchain import CleatCallbackHandler
    from langchain.agents import create_openai_functions_agent
    from langchain_openai import ChatOpenAI

    @cleat_entry
    def research_agent(h: HostCalls, topic: str) -> str:
        callback = CleatCallbackHandler(h)
        llm = ChatOpenAI(model="gpt-4o", callbacks=[callback])
        agent = create_openai_functions_agent(llm, tools, prompt)
        result = agent.invoke({"input": topic}, config={"callbacks": [callback]})
        return result["output"]
"""

from __future__ import annotations

import json
import time
from typing import Any, Optional
from uuid import UUID

from ..host_calls import HostCalls


class CleatCallbackHandler:
    """Records LangChain agent steps as cleat durable events.

    Implements the LangChain callback interface (duck-typed - no hard
    dependency on langchain). When langchain is installed, this can be
    used as a drop-in BaseCallbackHandler.

    Parameters
    ----------
    h : HostCalls
        The HostCalls instance for the current workflow execution.
    verbose : bool
        If True, each event is also logged via cleat_log for debugging.
    """

    def __init__(self, h: HostCalls, verbose: bool = False) -> None:
        self.h = h
        self.verbose = verbose
        self.step_counter = 0
        self._chain_starts: dict[str, float] = {}
        self._llm_starts: dict[str, float] = {}
        self._tool_starts: dict[str, float] = {}

    # ------------------------------------------------------------------
    # LLM callbacks
    # ------------------------------------------------------------------

    def on_llm_start(
        self,
        serialized: dict[str, Any],
        prompts: list[str],
        *,
        run_id: Optional[UUID] = None,
        parent_run_id: Optional[UUID] = None,
        tags: Optional[list[str]] = None,
        metadata: Optional[dict[str, Any]] = None,
        **kwargs: Any,
    ) -> None:
        """Called when an LLM call starts."""
        self.step_counter += 1
        rid = str(run_id) if run_id else ""
        self._llm_starts[rid] = time.time()

        self.h.set_state(
            f"langchain_step_{self.step_counter}_llm_start",
            {
                "class": serialized.get(
                    "name",
                    (
                        serialized.get("id", ["unknown"])[-1]
                        if isinstance(serialized.get("id"), list)
                        else "unknown"
                    ),
                ),
                "prompts": [p[:500] for p in prompts],  # Truncate for state size
                "run_id": rid,
                "step": self.step_counter,
            },
        )
        if self.verbose:
            self.h.cleat_log(
                f"[Cleat] LLM start step={self.step_counter} model={serialized.get('name', '?')}"
            )

    def on_llm_end(
        self,
        response: Any,
        *,
        run_id: Optional[UUID] = None,
        parent_run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when an LLM call ends."""
        rid = str(run_id) if run_id else ""
        elapsed = 0.0
        if rid in self._llm_starts:
            elapsed = time.time() - self._llm_starts.pop(rid)

        # Extract response info duck-typed (works with langchain and openai)
        resp_info = self._extract_llm_response(response)

        self.h.set_state(
            f"langchain_step_{self.step_counter}_llm_end",
            {
                "run_id": rid,
                "elapsed_ms": int(elapsed * 1000),
                "response": resp_info,
            },
        )
        if self.verbose:
            self.h.cleat_log(
                f"[Cleat] LLM end step={self.step_counter} elapsed={elapsed:.2f}s"
            )

    def on_llm_error(
        self,
        error: BaseException,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when an LLM call errors."""
        rid = str(run_id) if run_id else ""
        self._llm_starts.pop(rid, None)

        self.h.set_state(
            f"langchain_step_{self.step_counter}_llm_error",
            {"run_id": rid, "error": str(error)},
        )
        if self.verbose:
            self.h.cleat_log(f"[Cleat] LLM error step={self.step_counter}: {error}")

    def on_llm_new_token(
        self,
        token: str,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called for each new token during streaming (no-op for state size)."""
        # Streaming tokens are too fine-grained for event recording.
        # The final on_llm_end captures the complete response.
        pass

    # ------------------------------------------------------------------
    # Tool callbacks
    # ------------------------------------------------------------------

    def on_tool_start(
        self,
        serialized: dict[str, Any],
        input_str: str,
        *,
        run_id: Optional[UUID] = None,
        parent_run_id: Optional[UUID] = None,
        tags: Optional[list[str]] = None,
        metadata: Optional[dict[str, Any]] = None,
        inputs: Optional[dict[str, Any]] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a tool invocation starts."""
        self.step_counter += 1
        rid = str(run_id) if run_id else ""
        self._tool_starts[rid] = time.time()

        tool_name = serialized.get("name", "unknown")

        self.h.set_state(
            f"langchain_step_{self.step_counter}_tool_start",
            {
                "tool": tool_name,
                "input": input_str[:1000],  # Truncate for state size
                "run_id": rid,
                "step": self.step_counter,
            },
        )
        if self.verbose:
            self.h.cleat_log(
                f"[Cleat] Tool start step={self.step_counter} tool={tool_name}"
            )

    def on_tool_end(
        self,
        output: Any,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a tool invocation ends."""
        rid = str(run_id) if run_id else ""
        elapsed = 0.0
        if rid in self._tool_starts:
            elapsed = time.time() - self._tool_starts.pop(rid)

        output_str = str(output)[:2000] if output else ""

        self.h.set_state(
            f"langchain_step_{self.step_counter}_tool_end",
            {
                "run_id": rid,
                "elapsed_ms": int(elapsed * 1000),
                "output": output_str,
            },
        )
        if self.verbose:
            self.h.cleat_log(
                f"[Cleat] Tool end step={self.step_counter} elapsed={elapsed:.2f}s"
            )

    def on_tool_error(
        self,
        error: BaseException,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a tool invocation errors."""
        rid = str(run_id) if run_id else ""
        self._tool_starts.pop(rid, None)

        self.h.set_state(
            f"langchain_step_{self.step_counter}_tool_error",
            {"run_id": rid, "error": str(error)},
        )
        if self.verbose:
            self.h.cleat_log(f"[Cleat] Tool error step={self.step_counter}: {error}")

    # ------------------------------------------------------------------
    # Chain callbacks
    # ------------------------------------------------------------------

    def on_chain_start(
        self,
        serialized: dict[str, Any],
        inputs: dict[str, Any],
        *,
        run_id: Optional[UUID] = None,
        parent_run_id: Optional[UUID] = None,
        tags: Optional[list[str]] = None,
        metadata: Optional[dict[str, Any]] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a chain starts executing."""
        rid = str(run_id) if run_id else ""
        self._chain_starts[rid] = time.time()

        chain_name = serialized.get(
            "name",
            (
                serialized.get("id", ["chain"])[-1]
                if isinstance(serialized.get("id"), list)
                else "chain"
            ),
        )

        if self.verbose:
            self.h.cleat_log(f"[Cleat] Chain start: {chain_name}")

    def on_chain_end(
        self,
        outputs: dict[str, Any],
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a chain finishes executing."""
        rid = str(run_id) if run_id else ""
        elapsed = 0.0
        if rid in self._chain_starts:
            elapsed = time.time() - self._chain_starts.pop(rid)

        if self.verbose:
            self.h.cleat_log(f"[Cleat] Chain end elapsed={elapsed:.2f}s")

    def on_chain_error(
        self,
        error: BaseException,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a chain errors."""
        rid = str(run_id) if run_id else ""
        self._chain_starts.pop(rid, None)

        if self.verbose:
            self.h.cleat_log(f"[Cleat] Chain error: {error}")

    # ------------------------------------------------------------------
    # Agent callbacks
    # ------------------------------------------------------------------

    def on_agent_action(
        self,
        action: Any,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when an agent decides on an action."""
        self.step_counter += 1

        # Extract action info duck-typed (works with various agent versions)
        tool = getattr(action, "tool", str(action))
        tool_input = getattr(action, "tool_input", "")
        log = getattr(action, "log", str(action))

        self.h.set_state(
            f"langchain_step_{self.step_counter}_agent_action",
            {
                "tool": str(tool),
                "tool_input": str(tool_input)[:1000],
                "log": log[:2000],
                "step": self.step_counter,
            },
        )
        if self.verbose:
            self.h.cleat_log(
                f"[Cleat] Agent action step={self.step_counter} tool={tool}"
            )

    def on_agent_finish(
        self,
        finish: Any,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when an agent finishes."""
        return_values = getattr(finish, "return_values", {})
        log = getattr(finish, "log", str(finish))

        output = return_values.get("output", log)

        self.h.set_state(
            f"langchain_step_{self.step_counter}_agent_finish",
            {"output": str(output)[:5000], "step": self.step_counter},
        )
        if self.verbose:
            self.h.cleat_log(f"[Cleat] Agent finished step={self.step_counter}")

    # ------------------------------------------------------------------
    # Retriever callbacks
    # ------------------------------------------------------------------

    def on_retriever_start(
        self,
        serialized: dict[str, Any],
        query: str,
        *,
        run_id: Optional[UUID] = None,
        parent_run_id: Optional[UUID] = None,
        tags: Optional[list[str]] = None,
        metadata: Optional[dict[str, Any]] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a retriever starts."""
        self.step_counter += 1

        self.h.set_state(
            f"langchain_step_{self.step_counter}_retriever_start",
            {
                "query": query[:1000],
                "step": self.step_counter,
            },
        )

    def on_retriever_end(
        self,
        documents: Any,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a retriever ends."""
        # Extract document count duck-typed
        try:
            doc_count = len(documents)
        except TypeError:
            doc_count = 1

        self.h.set_state(
            f"langchain_step_{self.step_counter}_retriever_end",
            {"document_count": doc_count},
        )

    def on_retriever_error(
        self,
        error: BaseException,
        *,
        run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called when a retriever errors."""
        self.h.set_state(
            f"langchain_step_{self.step_counter}_retriever_error",
            {"error": str(error)},
        )

    # ------------------------------------------------------------------
    # Text callbacks (for newer LangChain versions)
    # ------------------------------------------------------------------

    def on_text(
        self,
        text: str,
        *,
        run_id: Optional[UUID] = None,
        parent_run_id: Optional[UUID] = None,
        **kwargs: Any,
    ) -> None:
        """Called on arbitrary text (no-op)."""
        pass

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _extract_llm_response(response: Any) -> dict[str, Any]:
        """Extract info from an LLM response duck-typed (no hard langchain dep)."""
        result: dict[str, Any] = {}

        # Try to get generations
        if hasattr(response, "generations"):
            gens = response.generations
            if gens:
                flat = []
                for g in gens:
                    if isinstance(g, list):
                        for sub in g:
                            if hasattr(sub, "text"):
                                flat.append(sub.text[:500])
                            elif hasattr(sub, "message"):
                                msg = sub.message
                                if hasattr(msg, "content"):
                                    flat.append(str(msg.content)[:500])
                                else:
                                    flat.append(str(msg)[:500])
                    elif hasattr(g, "text"):
                        flat.append(g.text[:500])
                result["generations"] = flat

        # Try to get token usage
        if hasattr(response, "llm_output"):
            llm_out = response.llm_output
            if isinstance(llm_out, dict):
                usage = llm_out.get("token_usage", {})
                if usage:
                    result["token_usage"] = usage

        return result
