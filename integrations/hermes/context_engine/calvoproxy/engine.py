"""Hermes context engine that keeps conversation ownership in Hermes."""

from __future__ import annotations

import json
import logging
import os
import shutil
import subprocess
import threading
import time
from typing import Any

from agent.context_compressor import ContextCompressor

logger = logging.getLogger(__name__)
TOOL_PROTOCOL = "calvoproxy.tool-compression.v1"


def _env_int(name: str, default: int, minimum: int = 0) -> int:
    try:
        return max(minimum, int(os.environ.get(name, default)))
    except (TypeError, ValueError):
        return default


def _env_float(name: str, default: float, minimum: float = 0.0) -> float:
    try:
        return max(minimum, float(os.environ.get(name, default)))
    except (TypeError, ValueError):
        return default


def _safe_anthropic_content(before: Any, after: Any) -> bool:
    """Allow edits only inside tool_result content, never adjacent user blocks."""
    if not isinstance(before, list) or not isinstance(after, list):
        return False
    if len(before) != len(after):
        return False
    changed_tool_result = False
    for old_block, new_block in zip(before, after):
        if old_block == new_block:
            continue
        if not isinstance(old_block, dict) or not isinstance(new_block, dict):
            return False
        if (
            old_block.get("type") != "tool_result"
            or new_block.get("type") != "tool_result"
        ):
            return False
        if set(old_block) != set(new_block):
            return False
        for key in old_block:
            if key != "content" and old_block[key] != new_block[key]:
                return False
        changed_tool_result = True
    return changed_tool_result


def _safe_bridge_messages(
    original: list[dict[str, Any]], candidate: Any
) -> list[dict[str, Any]] | None:
    """Reject a bridge result that changed anything except tool-result content."""
    if not isinstance(candidate, list) or len(candidate) != len(original):
        return None
    checked: list[dict[str, Any]] = []
    for before, after in zip(original, candidate):
        if not isinstance(before, dict) or not isinstance(after, dict):
            return None
        if set(before) != set(after):
            return None
        if before.get("role") != after.get("role"):
            return None
        for key in before:
            if key != "content" and before[key] != after[key]:
                return None
        if before.get("content") != after.get("content"):
            if before.get("role") == "tool":
                pass
            elif before.get("role") != "user" or not _safe_anthropic_content(
                before.get("content"), after.get("content")
            ):
                return None
        checked.append(after)
    return checked


class CalvoProxyContextEngine(ContextCompressor):
    """Proactive native Hermes compression plus local cervo-compress pruning.

    The class subclasses Hermes' built-in compressor deliberately: session DB
    mutation, summary-model calls, redaction, retry handling, and all native
    safety fences remain owned by Hermes. CalvoProxy contributes a lower token
    trigger, explicit local cooldown/in-flight guards, deterministic tool-result
    preprocessing, and a validated six-field handoff shape.
    """

    def __init__(self) -> None:
        trigger_tokens = _env_int("CALVOPROXY_COMPACTION_TRIGGER_TOKENS", 12_000, 1)
        threshold_percent = min(
            0.95,
            _env_float("CALVOPROXY_COMPACTION_THRESHOLD_PERCENT", 0.50, 0.05),
        )
        self._compaction_cooldown_seconds = _env_float(
            "CALVOPROXY_COMPACTION_COOLDOWN_SECONDS", 60.0
        )
        self._prune_cooldown_seconds = _env_float(
            "CALVOPROXY_TOOL_PRUNE_COOLDOWN_SECONDS", 10.0
        )
        self._tool_result_limit = _env_int(
            "CALVOPROXY_TOOL_RESULT_LIMIT", 4096, 512
        )
        self._tool_prune_tokens = _env_int(
            "CALVOPROXY_TOOL_PRUNE_TRIGGER_TOKENS", 6000, 1
        )
        self._bridge_timeout_seconds = _env_float(
            "CALVOPROXY_COMPRESSION_BRIDGE_TIMEOUT_SECONDS", 5.0, 0.1
        )
        self._compaction_guard = threading.Lock()
        self._compaction_cooldown_until = 0.0
        self._last_prune_at = 0.0
        super().__init__(
            model="coding",
            threshold_percent=threshold_percent,
            threshold_tokens_cap=trigger_tokens,
            proactive_prune_tokens=0,
            abort_on_summary_failure=False,
        )

    @property
    def name(self) -> str:
        return "calvoproxy"

    def should_compress_info(
        self, prompt_tokens: int | None = None
    ) -> tuple[bool, str | None]:
        decision, reason = super().should_compress_info(prompt_tokens)
        if not decision:
            return decision, reason
        if self._compaction_guard.locked():
            return False, "in_flight"
        remaining = self._compaction_cooldown_until - time.monotonic()
        if remaining > 0:
            return False, f"cooldown:{remaining:.0f}"
        return True, None

    def compress(
        self,
        messages: list[dict[str, Any]],
        current_tokens: int | None = None,
        focus_topic: str | None = None,
        force: bool = False,
        memory_context: str = "",
    ) -> list[dict[str, Any]]:
        if not self._compaction_guard.acquire(blocking=False):
            return messages
        try:
            if not force and time.monotonic() < self._compaction_cooldown_until:
                return messages
            try:
                prepared, _ = self._preprocess_old_tool_results(messages)
            except Exception:
                logger.warning(
                    "cervo-compress preprocessing failed; preserving original tool results",
                    exc_info=True,
                )
                prepared = messages
            compacted = super().compress(
                prepared,
                current_tokens=current_tokens,
                focus_topic=focus_topic,
                force=force,
                memory_context=memory_context,
            )
            if getattr(self, "_last_compression_made_progress", False):
                self._compaction_cooldown_until = (
                    time.monotonic() + self._compaction_cooldown_seconds
                )
            return compacted
        finally:
            self._compaction_guard.release()

    def prune_tool_results_only(
        self,
        messages: list[dict[str, Any]],
        current_tokens: int | None = None,
    ) -> tuple[list[dict[str, Any]], int]:
        if current_tokens is not None and current_tokens < self._tool_prune_tokens:
            return messages, 0
        if time.monotonic() - self._last_prune_at < self._prune_cooldown_seconds:
            return messages, 0
        if not self._compaction_guard.acquire(blocking=False):
            return messages, 0
        try:
            processed, changed = self._preprocess_old_tool_results(messages)
            self._last_prune_at = time.monotonic()
            return (processed, changed) if changed else (messages, 0)
        finally:
            self._compaction_guard.release()

    def _preprocess_old_tool_results(
        self, messages: list[dict[str, Any]]
    ) -> tuple[list[dict[str, Any]], int]:
        boundary = max(0, len(messages) - max(1, int(self.protect_last_n)))
        if boundary == 0:
            return messages, 0
        prefix = messages[:boundary]
        processed = self._call_cervo_bridge(prefix)
        if processed is None or processed == prefix:
            return messages, 0
        changed = sum(before != after for before, after in zip(prefix, processed))
        return processed + messages[boundary:], changed

    def _call_cervo_bridge(
        self, messages: list[dict[str, Any]]
    ) -> list[dict[str, Any]] | None:
        configured = os.environ.get("CALVOPROXY_BIN", "").strip()
        executable = configured or shutil.which("calvoproxy")
        if not executable:
            return None
        request = {
            "version": TOOL_PROTOCOL,
            "messages": messages,
            "tool_result_limit": self._tool_result_limit,
        }
        try:
            completed = subprocess.run(
                [executable, "compact-tools"],
                input=json.dumps(request, ensure_ascii=False),
                capture_output=True,
                text=True,
                timeout=self._bridge_timeout_seconds,
                check=False,
            )
            if completed.returncode != 0:
                logger.warning(
                    "cervo-compress bridge returned exit %d; preserving original tool results",
                    completed.returncode,
                )
                return None
            response = json.loads(completed.stdout)
            if response.get("version") != TOOL_PROTOCOL:
                return None
            return _safe_bridge_messages(messages, response.get("messages"))
        except (OSError, subprocess.SubprocessError, ValueError, TypeError):
            logger.warning(
                "cervo-compress bridge unavailable; preserving original tool results",
                exc_info=True,
            )
            return None

    def _generate_summary(
        self,
        turns_to_summarize: list[dict[str, Any]],
        focus_topic: str | None = None,
        memory_context: str = "",
    ) -> str | None:
        native = super()._generate_summary(
            turns_to_summarize,
            focus_topic=focus_topic,
            memory_context=memory_context,
        )
        if not native:
            return native
        # Lazy by design: Hermes' repo-plugin loader preloads sibling modules
        # before it registers this package. A module-level relative import can
        # therefore leave a half-initialized engine in sys.modules.
        from .summary import canonicalize_native_summary

        structured = canonicalize_native_summary(native)
        if structured is None:
            logger.warning(
                "CalvoProxy summary validation failed; using Hermes native summary"
            )
            return native
        return self._with_summary_prefix(structured)

    def on_session_reset(self) -> None:
        super().on_session_reset()
        self._compaction_cooldown_until = 0.0
        self._last_prune_at = 0.0
