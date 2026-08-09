"""Validation and canonicalization for CalvoProxy Hermes handoff summaries."""

from __future__ import annotations

import re
from collections.abc import Mapping


SUMMARY_PROTOCOL = "calvoproxy.compaction-summary.v1"
SUMMARY_PREFIXES = (
    "[CONTEXT COMPACTION]",
    "[CONTEXT SUMMARY]:",
    "[CONTEXT SUMMARY]",
)
REQUIRED_FIELDS = (
    "Objective",
    "Progress",
    "Constraints",
    "Files",
    "Blockers",
    "Next Action",
)
_HEADING_RE = re.compile(r"(?m)^##\s+([^\r\n]+?)\s*$")


def _sections(summary: str) -> dict[str, str]:
    matches = list(_HEADING_RE.finditer(summary))
    sections: dict[str, str] = {}
    for index, match in enumerate(matches):
        start = match.end()
        end = matches[index + 1].start() if index + 1 < len(matches) else len(summary)
        key = re.sub(r"\s+", " ", match.group(1).strip()).casefold()
        value = summary[start:end].strip()
        if value and key not in sections:
            sections[key] = value
    return sections


def _first(sections: Mapping[str, str], *names: str) -> str:
    for name in names:
        value = sections.get(name.casefold(), "").strip()
        if value:
            return value
    return ""


def _join_labeled(sections: Mapping[str, str], names: tuple[str, ...]) -> str:
    parts: list[str] = []
    for name in names:
        value = _first(sections, name)
        if value:
            parts.append(f"### {name}\n{value}")
    return "\n\n".join(parts)


def canonicalize_native_summary(native_summary: str) -> str | None:
    """Map a validated Hermes-native checkpoint to the small shared schema.

    Hermes remains the summarizer. This function only reshapes its richer
    checkpoint into the six fields shared with the Pi integration. A missing
    source field returns ``None`` so the caller can use the untouched native
    summary instead of committing a partial handoff.
    """
    if not isinstance(native_summary, str) or not native_summary.strip():
        return None
    sections = _sections(native_summary)
    fields = {
        "Objective": _first(sections, "Goal"),
        "Progress": _join_labeled(
            sections,
            (
                "Completed Actions",
                "Active State",
                "Key Decisions",
                "Resolved Questions",
                "Critical Context",
            ),
        ),
        "Constraints": _join_labeled(
            sections,
            ("Constraints & Preferences", "Pruned Skills To Reload"),
        ),
        "Files": _first(sections, "Relevant Files"),
        "Blockers": _first(sections, "Blocked"),
        "Next Action": _first(
            sections,
            "Historical Task Snapshot",
            "Active Task",
            "Remaining Work",
        ),
    }
    if any(not fields[name].strip() for name in REQUIRED_FIELDS):
        return None

    body = "\n\n".join(f"## {name}\n{fields[name].strip()}" for name in REQUIRED_FIELDS)
    candidate = f"{SUMMARY_PREFIXES[0]}\nProtocol: {SUMMARY_PROTOCOL}\n\n{body}"
    return candidate if validate_structured_summary(candidate) else None


def validate_structured_summary(summary: str) -> bool:
    """Require one non-empty instance of every v1 field and a size bound."""
    if not isinstance(summary, str) or len(summary.encode("utf-8")) > 64 * 1024:
        return False
    if not summary.startswith(SUMMARY_PREFIXES[0]):
        return False
    if f"Protocol: {SUMMARY_PROTOCOL}" not in summary[:256]:
        return False
    sections = _sections(summary)
    if set(sections) != {name.casefold() for name in REQUIRED_FIELDS}:
        return False
    return all(sections[name.casefold()].strip() for name in REQUIRED_FIELDS)
