"""CleatCheckpointer: Uses cleat cleat state as LangGraph's checkpoint backend.

Usage::

    from cleat_sdk import HostCalls, cleat_entry
    from cleat_sdk.langgraph import CleatCheckpointer
    from langgraph.graph import StateGraph

    @cleat_entry
    def my_agent(h: HostCalls, input: str) -> str:
        checkpointer = CleatCheckpointer(h)
        graph = StateGraph(MyState)
        # ... build graph ...
        app = graph.compile(checkpointer=checkpointer)
        result = app.invoke({"input": input}, config={"configurable": {"thread_id": "1"}})
        return result

Design notes
------------
* Checkpoint data is serialized to JSON and stored via ``h.set_state()``.
* The state key pattern is ``langgraph_ckpt_{thread_id}`` for checkpoints.
* Writes use ``langgraph_write_{thread_id}_{task_id}``.
* All data is serialized as JSON since cleat state uses JSON-based storage.
* This is a **duck-typed** implementation: it matches LangGraph's
  ``BaseCheckpointSaver`` method signatures without importing langgraph.
"""

from __future__ import annotations

import json
from typing import Any, Optional


class CleatCheckpointer:
    """LangGraph checkpointer backed by cleat cleat state.

    Stores LangGraph checkpoints, intermediate writes, and metadata
    in cleat's cleat state.  When the workflow resumes from a crash,
    the last checkpoint is restored and execution continues.

    Implements the LangGraph ``BaseCheckpointSaver`` interface (duck-typed).
    No hard dependency on langgraph.

    Parameters
    ----------
    h : HostCalls
        The ``HostCalls`` instance for the current workflow execution.
    """

    def __init__(self, h: Any) -> None:
        # Duck-typed: accept any object with set_state / get_state / list_state.
        self._h = h

    # ------------------------------------------------------------------
    # Checkpoint retrieval
    # ------------------------------------------------------------------

    def get_tuple(self, config: dict[str, Any]) -> Optional[Any]:
        """Get a checkpoint tuple from cleat state.

        Parameters
        ----------
        config : dict
            LangGraph config dict.  Must contain
            ``config["configurable"]["thread_id"]``.

        Returns
        -------
        CheckpointTuple or None
            The checkpoint tuple if found, or ``None``.
        """
        thread_id = self._get_thread_id(config)
        if thread_id is None:
            return None

        key = f"langgraph_ckpt_{thread_id}"
        try:
            data = self._h.get_state(key, dict)
        except Exception:
            return None

        if not data:
            return None

        return self._deserialize_checkpoint_tuple(data)

    # ------------------------------------------------------------------
    # Checkpoint persistence
    # ------------------------------------------------------------------

    def put(
        self,
        config: dict[str, Any],
        checkpoint: Any,
        metadata: dict[str, Any],
        new_versions: dict[str, Any],
    ) -> dict[str, Any]:
        """Save a checkpoint to cleat state.

        Parameters
        ----------
        config : dict
            LangGraph config with ``thread_id``.
        checkpoint : Any
            The checkpoint object to save.
        metadata : dict
            Checkpoint metadata (source, step, writes, etc.).
        new_versions : dict
            Channel versions.

        Returns
        -------
        dict
            The config with ``checkpoint_id`` updated.
        """
        thread_id = self._get_thread_id(config)
        if thread_id is None:
            return config

        # Serialise checkpoint and metadata to JSON-safe dicts.
        ckpt_data = _make_json_safe(self._serialize_checkpoint(checkpoint))
        metadata_data = _make_json_safe(metadata)

        key = f"langgraph_ckpt_{thread_id}"
        stored = {
            "checkpoint": ckpt_data,
            "metadata": metadata_data,
            "new_versions": new_versions,
        }
        self._h.set_state(key, json.dumps(stored))

        # Extract checkpoint_id from metadata or checkpoint object.
        checkpoint_id = metadata.get("checkpoint_id") or metadata.get("id") or ""
        if not checkpoint_id and hasattr(checkpoint, "id"):
            checkpoint_id = checkpoint.id

        return {
            **config,
            "configurable": {
                **config.get("configurable", {}),
                "checkpoint_id": checkpoint_id,
            },
        }

    # ------------------------------------------------------------------
    # Intermediate writes (pending writes)
    # ------------------------------------------------------------------

    def put_writes(
        self,
        config: dict[str, Any],
        writes: Any,
        task_id: str,
    ) -> None:
        """Save intermediate writes for a task.

        Parameters
        ----------
        config : dict
            LangGraph config with ``thread_id``.
        writes : Any
            The writes to save (sequence of channel updates).
        task_id : str
            The task ID these writes belong to.
        """
        thread_id = self._get_thread_id(config)
        if thread_id is None:
            return

        key = f"langgraph_write_{thread_id}_{task_id}"
        writes_data = _make_json_safe(self._serialize_writes(writes))
        self._h.set_state(key, json.dumps(writes_data))

    def get_writes(self, config: dict[str, Any]) -> list[Any]:
        """Get all pending writes for a thread.

        Parameters
        ----------
        config : dict
            LangGraph config with ``thread_id``.

        Returns
        -------
        list
            List of pending writes.
        """
        thread_id = self._get_thread_id(config)
        if thread_id is None:
            return []

        prefix = f"langgraph_write_{thread_id}_"
        try:
            keys = self._h.list_state(prefix)
        except Exception:
            return []

        writes: list[Any] = []
        for k in keys:
            try:
                raw = self._h.get_state(k, dict)
                if raw:
                    writes.append(raw)
            except Exception:
                pass
        return writes

    # ------------------------------------------------------------------
    # Listing
    # ------------------------------------------------------------------

    def list(
        self,
        config: Optional[dict[str, Any]],
        *,
        filter: Optional[dict[str, Any]] = None,
        before: Optional[dict[str, Any]] = None,
        limit: Optional[int] = None,
    ) -> list[Any]:
        """List checkpoints for a thread.

        Parameters
        ----------
        config : dict or None
            LangGraph config with ``thread_id``.
        filter : dict or None
            Optional metadata filter (not yet implemented).
        before : dict or None
            Return checkpoints before this config (not yet implemented).
        limit : int or None
            Maximum number of checkpoints to return.

        Returns
        -------
        list[CheckpointTuple]
            List of checkpoint tuples.
        """
        if config is None:
            return []

        thread_id = self._get_thread_id(config)
        if thread_id is None:
            return []

        prefix = f"langgraph_ckpt_{thread_id}"
        try:
            keys = self._h.list_state(prefix)
        except Exception:
            return []

        results: list[Any] = []
        for k in keys:
            try:
                raw = self._h.get_state(k, dict)
                if raw:
                    ckpt_tuple = self._deserialize_checkpoint_tuple(raw)
                    if ckpt_tuple is not None:
                        results.append(ckpt_tuple)
            except Exception:
                pass

        # Sort by checkpoint_id descending (newest first).
        results.sort(
            key=lambda t: getattr(t, "checkpoint_id", "") or "",
            reverse=True,
        )

        if limit is not None and limit > 0:
            results = results[:limit]

        return results

    # ------------------------------------------------------------------
    # Helpers
    # ------------------------------------------------------------------

    @staticmethod
    def _get_thread_id(config: dict[str, Any]) -> Optional[str]:
        """Extract ``thread_id`` from a LangGraph config."""
        if not isinstance(config, dict):
            return None
        cfg = config.get("configurable", {})
        if not isinstance(cfg, dict):
            return None
        return cfg.get("thread_id")

    @staticmethod
    def _serialize_checkpoint(checkpoint: Any) -> dict[str, Any]:
        """Serialize a LangGraph checkpoint to a dict."""
        if hasattr(checkpoint, "__dict__"):
            return dict(checkpoint.__dict__)
        if isinstance(checkpoint, dict):
            return dict(checkpoint)
        return {"data": str(checkpoint)}

    @staticmethod
    def _serialize_writes(writes: Any) -> list[Any]:
        """Serialize intermediate writes to a list."""
        if isinstance(writes, (list, tuple)):
            return list(writes)
        return [writes]

    @staticmethod
    def _deserialize_checkpoint_tuple(data: Any) -> Any:
        """Deserialize stored data into a duck-typed CheckpointTuple.

        The returned object has the attributes LangGraph expects:
        ``config``, ``checkpoint``, ``metadata``, ``parent_config``,
        and ``pending_writes``.
        """
        # If data is a JSON string, parse it.
        if isinstance(data, str):
            try:
                data = json.loads(data)
            except (json.JSONDecodeError, TypeError):
                return None
        if not isinstance(data, dict):
            return None

        ckpt_part = data.get("checkpoint", {})
        meta = data.get("metadata", {})
        versions = data.get("new_versions", {})

        # Ensure checkpoint part is a dict.
        if isinstance(ckpt_part, str):
            try:
                ckpt_part = json.loads(ckpt_part)
            except (json.JSONDecodeError, TypeError):
                ckpt_part = {}
        if not isinstance(ckpt_part, dict):
            ckpt_part = {}

        # Ensure metadata is a dict.
        if isinstance(meta, str):
            try:
                meta = json.loads(meta)
            except (json.JSONDecodeError, TypeError):
                meta = {}
        if not isinstance(meta, dict):
            meta = {}

        # ------------------------------------------------------------------
        # Build a duck-typed CheckpointTuple.
        # ------------------------------------------------------------------

        class _CkptTuple:
            """Duck-typed CheckpointTuple."""

            __slots__ = (
                "config",
                "checkpoint",
                "metadata",
                "parent_config",
                "pending_writes",
                "checkpoint_id",
            )

        result = _CkptTuple()

        tid = meta.get("thread_id", "")
        cid = meta.get("checkpoint_id") or meta.get("id") or ""

        result.config = {
            "configurable": {
                "thread_id": tid,
                "checkpoint_id": cid,
            },
        }
        result.checkpoint_id = cid

        # Build a duck-typed checkpoint object with channel values.
        class _Ckpt:
            """Duck-typed checkpoint."""

            __slots__ = ("channel_values", "channel_versions", "id")

        ckpt = _Ckpt()
        ckpt.channel_values = ckpt_part.get("channel_values", {})
        ckpt.channel_versions = versions if isinstance(versions, dict) else {}
        ckpt.id = cid

        result.checkpoint = ckpt
        result.metadata = meta
        result.parent_config = None
        result.pending_writes = []

        return result


# ======================================================================
# Module-level helpers
# ======================================================================


def _make_json_safe(obj: Any) -> Any:
    """Recursively convert an object to JSON-safe Python types.

    * ``None``, ``bool``, ``int``, ``float``, ``str`` are returned as-is.
    * ``list`` / ``tuple`` are mapped recursively.
    * ``dict`` keys are converted to ``str``.
    * Objects with ``__dict__`` are converted to dicts (skipping private attrs).
    * Everything else is converted to ``str``.
    """
    if obj is None:
        return None
    if isinstance(obj, (bool, int, float, str)):
        return obj
    if isinstance(obj, (list, tuple)):
        return [_make_json_safe(item) for item in obj]
    if isinstance(obj, dict):
        return {str(k): _make_json_safe(v) for k, v in obj.items()}
    if hasattr(obj, "__dict__"):
        return {
            str(k): _make_json_safe(v) for k, v in obj.__dict__.items() if not k.startswith("_")
        }
    return str(obj)
