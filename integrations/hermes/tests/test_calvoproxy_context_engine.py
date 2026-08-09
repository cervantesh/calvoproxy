from __future__ import annotations

import sys
import threading
import unittest
from pathlib import Path
from types import ModuleType


class _NativeContextCompressor:
    """Small Hermes contract double; integration tests use the real checkout."""

    def __init__(self, **kwargs):
        self.protect_last_n = 2
        self.threshold_tokens = kwargs.get("threshold_tokens_cap", 12_000)
        self._last_compression_made_progress = False
        self.native_summary = ""

    def should_compress_info(self, prompt_tokens=None):
        return ((prompt_tokens or 0) >= self.threshold_tokens, None)

    def compress(self, messages, **kwargs):
        return messages

    def _generate_summary(self, turns_to_summarize, **kwargs):
        return self.native_summary

    def _with_summary_prefix(self, summary):
        return summary

    def on_session_reset(self):
        return None


agent_module = ModuleType("agent")
compressor_module = ModuleType("agent.context_compressor")
compressor_module.ContextCompressor = _NativeContextCompressor
agent_module.context_compressor = compressor_module
sys.modules.setdefault("agent", agent_module)
sys.modules.setdefault("agent.context_compressor", compressor_module)

PLUGIN_PARENT = Path(__file__).resolve().parents[1] / "context_engine"
sys.path.insert(0, str(PLUGIN_PARENT))

from calvoproxy.engine import (  # noqa: E402
    CalvoProxyContextEngine,
    _safe_bridge_messages,
)
from calvoproxy.summary import (  # noqa: E402
    SUMMARY_PROTOCOL,
    canonicalize_native_summary,
    validate_structured_summary,
)


NATIVE_SUMMARY = """[CONTEXT COMPACTION]
## Historical Task Snapshot
Implement the adapter and run its tests.

## Goal
Keep CalvoProxy stateless while Hermes owns compaction.

## Constraints & Preferences
- Do not send transcript content as metadata.

## Completed Actions
1. Read the Hermes context engine contract.

## Active State
- Implementation is local and uncommitted.

## Blocked
None.

## Key Decisions
- Delegate session mutation to Hermes.

## Resolved Questions
None.

## Relevant Files
- integrations/hermes/context_engine/calvoproxy/engine.py

## Critical Context
Use the local bridge only.
"""


class SummaryTests(unittest.TestCase):
    def test_native_summary_maps_to_exact_shared_schema(self):
        structured = canonicalize_native_summary(NATIVE_SUMMARY)
        self.assertIsNotNone(structured)
        self.assertTrue(validate_structured_summary(structured))
        self.assertIn(f"Protocol: {SUMMARY_PROTOCOL}", structured)
        for heading in (
            "Objective",
            "Progress",
            "Constraints",
            "Files",
            "Blockers",
            "Next Action",
        ):
            self.assertEqual(structured.count(f"## {heading}\n"), 1)

    def test_missing_required_native_field_forces_native_fallback(self):
        missing_blockers = NATIVE_SUMMARY.replace("## Blocked\nNone.\n\n", "")
        self.assertIsNone(canonicalize_native_summary(missing_blockers))

    def test_engine_returns_untouched_native_summary_when_validation_fails(self):
        engine = CalvoProxyContextEngine()
        engine.native_summary = "[CONTEXT COMPACTION]\n## Goal\nOnly one field."
        self.assertEqual(engine._generate_summary([]), engine.native_summary)


class BridgeValidationTests(unittest.TestCase):
    def test_only_tool_result_content_may_change(self):
        original = [
            {"role": "user", "content": "question"},
            {"role": "tool", "content": "verbose", "tool_call_id": "call-1"},
        ]
        valid = [
            {"role": "user", "content": "question"},
            {"role": "tool", "content": "short", "tool_call_id": "call-1"},
        ]
        self.assertEqual(_safe_bridge_messages(original, valid), valid)
        invalid = [dict(valid[0], content="rewritten question"), valid[1]]
        self.assertIsNone(_safe_bridge_messages(original, invalid))

    def test_anthropic_tool_result_content_may_change_without_envelope_loss(self):
        original = [{
            "role": "user",
            "content": [{
                "type": "tool_result",
                "tool_use_id": "toolu-1",
                "content": "verbose",
            }],
        }]
        candidate = [{
            "role": "user",
            "content": [{
                "type": "tool_result",
                "tool_use_id": "toolu-1",
                "content": "short",
            }],
        }]
        self.assertEqual(_safe_bridge_messages(original, candidate), candidate)

        candidate[0]["content"].append({"type": "text", "text": "injected"})
        self.assertIsNone(_safe_bridge_messages(original, candidate))


class LifecycleTests(unittest.TestCase):
    def test_prune_protects_recent_tail_and_commits_only_changed_prefix(self):
        engine = CalvoProxyContextEngine()
        messages = [
            {"role": "system", "content": "system"},
            {"role": "tool", "content": "old", "tool_call_id": "old"},
            {"role": "assistant", "content": "recent"},
            {"role": "tool", "content": "new", "tool_call_id": "new"},
        ]

        def bridge(prefix):
            changed = [dict(message) for message in prefix]
            changed[1] = dict(changed[1], content="compressed-old")
            return changed

        engine._call_cervo_bridge = bridge
        result, changed = engine.prune_tool_results_only(messages, current_tokens=7000)
        self.assertEqual(changed, 1)
        self.assertEqual(result[1]["content"], "compressed-old")
        self.assertIs(result[-1], messages[-1])

    def test_in_flight_and_cooldown_guards_block_proactive_trigger(self):
        engine = CalvoProxyContextEngine()
        engine._compaction_guard.acquire()
        try:
            self.assertEqual(engine.should_compress_info(12_000), (False, "in_flight"))
        finally:
            engine._compaction_guard.release()
        engine._compaction_cooldown_until = float("inf")
        decision, reason = engine.should_compress_info(12_000)
        self.assertFalse(decision)
        self.assertTrue(reason.startswith("cooldown:"))


if __name__ == "__main__":
    unittest.main()
