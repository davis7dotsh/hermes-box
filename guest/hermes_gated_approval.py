"""Conservative one-shot Codex reviewer for flagged Hermes commands.

This module is installed into ``tools/gated_approval.py`` by Hermes Box.  It
is intentionally independent from Hermes' mutable approval state: a model
verdict can approve only the invocation currently being checked.
"""

from __future__ import annotations

import contextvars
import json
import logging
import math
import os
import re
from typing import Any, Callable


logger = logging.getLogger(__name__)

_gate_context: contextvars.ContextVar[dict[str, Any] | None] = contextvars.ContextVar(
    "approval_gate_context", default=None
)
_secret_key = re.compile(
    r"(?:api[_-]?key|token|secret|password|authorization|cookie|credential)",
    re.IGNORECASE,
)
_secret_value = re.compile(
    r"(?i)\b(?=[a-z_][a-z0-9_.-]*\s*[:=])"
    r"(?=[a-z0-9_.-]*(?:api[_-]?key|token|secret|password|authorization|cookie|credential))"
    r"[a-z_][a-z0-9_.-]*\s*[:=]\s*(?:bearer\s+)?"
    r"(?:\"[^\"]*\"|'[^']*'|[^\s,;]+)|"
    r"\b(?:bearer\s+)[a-z0-9._~+/=-]{8,}|"
    r"\bsk-[a-z0-9_-]{8,}"
)
_risk_order = {"low": 0, "medium": 1, "high": 2, "critical": 3}

SYSTEM_PROMPT = """You are a conservative command-permission reviewer for an AI agent.
Return only one JSON object with exactly these fields:
decision (approve|escalate|deny), scope (once), risk
(low|medium|high|critical), confidence (0.0 to 1.0), and reason (short string).
Approve only when the exact invocation is clearly safe in the supplied task
context. Escalate when intent is missing, confidence is low, credentials may
be exposed, private data leaves the local machine, TLS is disabled, data is
mutated or deleted, or consequences are meaningful. Deny only clearly
malicious or catastrophic behavior. Never grant session or permanent scope."""


def set_current_gate_context(context: dict[str, Any] | None):
    return _gate_context.set(context or {})


def reset_current_gate_context(token) -> None:
    _gate_context.reset(token)


def append_gate_tool_event(event: dict[str, Any]) -> None:
    context = _gate_context.get()
    if not isinstance(context, dict):
        return
    events = context.setdefault("recent_tool_activity", [])
    if not isinstance(events, list):
        events = []
        context["recent_tool_activity"] = events
    events.append(_redact(event, depth=0))
    del events[:-12]


def _redact(value: Any, *, depth: int = 0) -> Any:
    if depth >= 6:
        return "[TRUNCATED]"
    if isinstance(value, dict):
        result = {}
        for index, (key, item) in enumerate(value.items()):
            if index >= 40:
                result["[TRUNCATED]"] = True
                break
            key_text = str(key)[:120]
            result[key_text] = (
                "[REDACTED]"
                if _secret_key.search(key_text)
                else _redact(item, depth=depth + 1)
            )
        return result
    if isinstance(value, (list, tuple)):
        return [_redact(item, depth=depth + 1) for item in value[:40]]
    if isinstance(value, str):
        return _secret_value.sub("[REDACTED]", value[:4000])
    if value is None or isinstance(value, (bool, int, float)):
        return value
    return _redact(str(value), depth=depth + 1)


def build_review_packet(
    command: str,
    warning: str,
    pattern_keys: list[str],
    max_context_chars: int,
) -> str:
    context = dict(_gate_context.get() or {})
    context.setdefault("cwd", os.getcwd())
    packet = {
        "command": _secret_value.sub("[REDACTED]", command),
        "warning": _secret_value.sub("[REDACTED]", warning),
        "pattern_keys": _redact(pattern_keys),
        "context": _redact(context),
    }
    serialized = json.dumps(packet, ensure_ascii=False, separators=(",", ":"))
    limit = max(1000, min(int(max_context_chars), 50000))
    if len(serialized) <= limit:
        return serialized
    # Preserve the exact command and warning while bounding optional context.
    packet["context"] = {"truncated": True}
    serialized = json.dumps(packet, ensure_ascii=False, separators=(",", ":"))
    if len(serialized) > limit:
        raise ValueError("exact command and warning exceed the review packet limit")
    return serialized


def _response_text(response: Any) -> str:
    if isinstance(response, str):
        return response
    if isinstance(response, dict):
        choices = response.get("choices")
        if choices:
            first = choices[0]
            if isinstance(first, dict):
                message = first.get("message")
                if isinstance(message, dict):
                    return str(message.get("content") or "")
        return str(response.get("content") or "")
    choices = getattr(response, "choices", None)
    if choices:
        return str(getattr(getattr(choices[0], "message", None), "content", "") or "")
    return ""


def normalize_verdict(raw: str, gate: dict[str, Any]) -> dict[str, Any]:
    try:
        parsed = json.loads(raw)
    except Exception as exc:
        return _escalation(f"reviewer returned malformed JSON: {type(exc).__name__}")
    required = {"decision", "scope", "risk", "confidence", "reason"}
    if not isinstance(parsed, dict) or set(parsed) != required:
        return _escalation("reviewer response did not match the strict schema")

    decision = parsed.get("decision")
    scope = parsed.get("scope")
    risk = parsed.get("risk")
    reason = parsed.get("reason")
    confidence = parsed.get("confidence")
    if decision not in {"approve", "escalate", "deny"}:
        return _escalation("reviewer returned an unsupported decision")
    if not isinstance(reason, str) or not reason.strip():
        return _escalation("reviewer omitted its reason")
    if not isinstance(confidence, (int, float)) or isinstance(confidence, bool):
        return _escalation("reviewer returned invalid confidence")
    confidence = float(confidence)
    if not math.isfinite(confidence) or confidence < 0 or confidence > 1:
        return _escalation("reviewer confidence was outside 0..1")
    if scope != "once":
        return _escalation("reviewer requested unsupported approval scope", risk, confidence)
    if risk not in _risk_order:
        return _escalation("reviewer returned unknown risk", risk, confidence)

    verdict = {
        "decision": decision,
        "scope": scope,
        "risk": risk,
        "confidence": confidence,
        "reason": reason.strip()[:500],
    }
    if decision != "approve":
        return verdict
    max_risk = str(gate.get("auto_approve_max_risk", "medium")).lower()
    if max_risk not in _risk_order:
        max_risk = "medium"
    if _risk_order[max_risk] > _risk_order["medium"]:
        max_risk = "medium"
    if _risk_order[risk] > _risk_order[max_risk]:
        return _escalation("reviewer risk exceeded the local approval ceiling", risk, confidence)
    try:
        minimum = float(gate.get("min_confidence", 0.75))
    except (TypeError, ValueError):
        minimum = 0.75
    minimum = max(minimum, 0.75)
    if confidence < minimum:
        return _escalation("reviewer confidence was below the local floor", risk, confidence)
    return verdict


def _escalation(
    reason: str, risk: str = "unknown", confidence: float = 0.0
) -> dict[str, Any]:
    return {
        "decision": "escalate",
        "scope": "once",
        "risk": risk,
        "confidence": confidence,
        "reason": reason,
    }


def review_command(
    command: str,
    warning: str,
    pattern_keys: list[str],
    gate: dict[str, Any],
    *,
    call: Callable[..., Any] | None = None,
) -> dict[str, Any]:
    if not gate.get("enabled", True):
        return _escalation("approval gate is disabled")
    if gate.get("scope", "once") != "once":
        return _escalation("configured approval scope is not one-shot")
    provider = str(gate.get("provider", "openai-codex"))
    model = str(gate.get("model", "gpt-5.5"))
    effort = str(gate.get("reasoning_effort", "low")).lower()
    tier = str(gate.get("service_tier", "fast")).lower()
    if tier == "fast":
        tier = "priority"
    try:
        timeout = float(gate.get("timeout", 30))
    except (TypeError, ValueError):
        timeout = 30.0
    try:
        max_chars = int(gate.get("max_context_chars", 12000))
    except (TypeError, ValueError):
        max_chars = 12000
    try:
        packet = build_review_packet(command, warning, pattern_keys, max_chars)
    except (TypeError, ValueError) as exc:
        logger.warning("Gated approval context was rejected: %s", exc)
        return _escalation("review context exceeded the configured limit")
    if call is None:
        from agent.auxiliary_client import call_llm

        call = call_llm
    logger.info("Auxiliary approval_gate: using %s (%s)", provider, model)
    try:
        response = call(
            task="approval_gate",
            provider=provider,
            model=model,
            messages=[
                {"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": packet},
            ],
            temperature=0,
            max_tokens=220,
            timeout=timeout,
            extra_body={
                "reasoning": {"effort": effort},
                "service_tier": tier,
            },
        )
        return normalize_verdict(_response_text(response), gate)
    except Exception as exc:
        logger.warning(
            "Gated approval reviewer failed; escalating (%s)", type(exc).__name__
        )
        return _escalation(f"reviewer error: {type(exc).__name__}")
