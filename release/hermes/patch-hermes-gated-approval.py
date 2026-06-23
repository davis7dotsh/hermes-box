#!/usr/bin/env python3
"""Install the Hermes Box gated-approval extension into a pinned checkout."""

from __future__ import annotations

import argparse
import hashlib
import shutil
import subprocess
import sys
from pathlib import Path

import yaml


EXPECTED_COMMIT = "2bd1977d8fad185c9b4be47884f7e87f1add0ce3"
EXPECTED_VERSION = 'version = "0.17.0"'


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise RuntimeError(f"upstream anchor drift for {label}: expected 1 match, found {count}")
    return text.replace(old, new, 1)


def patch_approval(path: Path) -> None:
    text = path.read_text()
    text = replace_once(
        text,
        '    # --- Phase 2.5: Smart approval (auxiliary LLM risk assessment) ---\n',
        '''    # --- Phase 2.5: conservative one-shot model gate ---
    gate_verdict = None
    if approval_mode == "gated":
        from tools.gated_approval import review_command
        combined_desc_for_gate = "; ".join(desc for _, desc, _ in warnings)
        gate_verdict = review_command(
            command,
            combined_desc_for_gate,
            [key for key, _, _ in warnings],
            _get_approval_config().get("gate", {}) or {},
        )
        if gate_verdict["decision"] == "approve":
            logger.info("Gated approval: auto-approved once")
            return {
                "approved": True,
                "message": None,
                "gate_approved": True,
                "gate_verdict": gate_verdict,
                "description": combined_desc_for_gate,
            }
        if gate_verdict["decision"] == "deny":
            return {
                "approved": False,
                "message": (
                    "BLOCKED by gated approval: " + gate_verdict["reason"] +
                    ". Do NOT retry or attempt to evade this decision."
                ),
                "gate_denied": True,
                "gate_verdict": gate_verdict,
                "user_consent": False,
            }

    # --- Phase 2.5: Smart approval (auxiliary LLM risk assessment) ---
''',
        "gated decision insertion",
    )
    text = replace_once(
        text,
        '                "allow_permanent": not has_tirith,\n',
        '''                "allow_permanent": not has_tirith,
                "gate_verdict": gate_verdict,
                "gate_reason": gate_verdict.get("reason") if gate_verdict else None,
''',
        "gateway verdict payload",
    )
    text = replace_once(
        text,
        '    combined_desc = "; ".join(desc for _, desc, _ in warnings)\n',
        '''    combined_desc = "; ".join(desc for _, desc, _ in warnings)
    if gate_verdict is not None:
        combined_desc += (
            "\\nGate verdict: escalate "
            f"(risk={gate_verdict.get('risk')}, "
            f"confidence={gate_verdict.get('confidence')}) — "
            f"{gate_verdict.get('reason')}"
        )
''',
        "human escalation reason",
    )
    path.write_text(text)


def patch_gateway(path: Path) -> None:
    text = path.read_text()
    text = replace_once(
        text,
        '''                register_gateway_notify,
                reset_current_session_key,
                set_current_session_key,
                unregister_gateway_notify,
''',
        '''                register_gateway_notify,
                reset_current_session_key,
                set_current_session_key,
                unregister_gateway_notify,
            )
            from tools.gated_approval import (
                reset_current_gate_context,
                set_current_gate_context,
''',
        "gateway gated imports",
    )
    text = replace_once(
        text,
        '''        def progress_callback(event_type: str, tool_name: str = None, preview: str = None, args: dict = None, **kwargs):
            """Callback invoked by agent on tool lifecycle events."""
''',
        '''        def progress_callback(event_type: str, tool_name: str = None, preview: str = None, args: dict = None, **kwargs):
            """Callback invoked by agent on tool lifecycle events."""
            from tools.gated_approval import append_gate_tool_event

            try:
                append_gate_tool_event({
                    "event": event_type,
                    "tool": tool_name,
                    "arguments": args,
                    "preview": preview,
                    "error": kwargs.get("error"),
                    "duration": kwargs.get("duration"),
                    "result_preview": kwargs.get("result_preview"),
                })
            except Exception as _gate_event_error:
                logger.debug("approval gate event capture failed: %s", _gate_event_error)
''',
        "tool event capture",
    )
    text = replace_once(
        text,
        '''            _approval_session_key = session_key or ""
            _approval_session_token = set_current_session_key(_approval_session_key)
''',
        '''            _approval_session_key = session_key or ""
            _gate_context_token = set_current_gate_context({
                "current_user_message": str(message)[:4000],
                "recent_messages": history[-6:],
                "task_id": session_id,
                "session_key_present": bool(_approval_session_key),
                "platform": platform_key,
                "chat_type": getattr(source, "chat_type", None),
                "chat_name": getattr(source, "chat_name", None),
                "thread_id": getattr(source, "thread_id", None),
                "recent_tool_activity": [],
            })
            _approval_session_token = set_current_session_key(_approval_session_key)
''',
        "gateway context hydration",
    )
    text = replace_once(
        text,
        '                reset_current_session_key(_approval_session_token)\n',
        '''                reset_current_session_key(_approval_session_token)
                reset_current_gate_context(_gate_context_token)
''',
        "gateway context reset",
    )
    path.write_text(text)


def patch_auxiliary_client(path: Path) -> None:
    text = path.read_text()
    text = replace_once(
        text,
        '''                    resp_kwargs["include"] = ["reasoning.encrypted_content"]

        # Tools support for auxiliary callers (e.g. skills_hub) that pass function schemas
''',
        '''                    resp_kwargs["include"] = ["reasoning.encrypted_content"]

            # Preserve the Responses API service tier selected by security-
            # sensitive auxiliary callers. The gated reviewer maps its
            # user-facing "fast" setting to the API's "priority" value.
            service_tier = extra_body.get("service_tier")
            if service_tier is not None:
                resp_kwargs["service_tier"] = service_tier

        # Tools support for auxiliary callers (e.g. skills_hub) that pass function schemas
''',
        "Codex service tier forwarding",
    )
    path.write_text(text)


def seed_config(path: Path) -> None:
    config = (yaml.safe_load(path.read_text()) or {}) if path.exists() else {}
    if not isinstance(config, dict):
        raise RuntimeError("Hermes config root is not a mapping")
    approvals = config.setdefault("approvals", {})
    if not isinstance(approvals, dict):
        raise RuntimeError("Hermes approvals config is not a mapping")
    approvals.update({"mode": "gated", "timeout": 60, "cron_mode": "deny", "gateway_timeout": 300})
    approvals["gate"] = {
        "enabled": True,
        "provider": "openai-codex",
        "model": "gpt-5.5",
        "reasoning_effort": "low",
        "service_tier": "fast",
        "timeout": 30,
        "scope": "once",
        "min_confidence": 0.75,
        "max_context_chars": 12000,
        "auto_approve_max_risk": "medium",
        "escalate_on_error": True,
        "escalate_on_low_confidence": True,
    }
    security = config.setdefault("security", {})
    if not isinstance(security, dict):
        raise RuntimeError("Hermes security config is not a mapping")
    security.update({"redact_secrets": True, "tirith_enabled": True})
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(yaml.safe_dump(config, sort_keys=False))


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--module", type=Path, required=True)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--commit", required=True)
    args = parser.parse_args()

    commit = args.commit
    if commit != EXPECTED_COMMIT:
        raise RuntimeError(
            f"unsupported Hermes revision {commit}; gated approval requires {EXPECTED_COMMIT}"
        )
    if EXPECTED_VERSION not in (args.source / "pyproject.toml").read_text():
        raise RuntimeError("unsupported Hermes version; expected 0.17.0")

    approval = args.source / "tools" / "approval.py"
    gateway = args.source / "gateway" / "run.py"
    auxiliary = args.source / "agent" / "auxiliary_client.py"
    target_module = args.source / "tools" / "gated_approval.py"
    marker = args.source / ".hermes-box-gated-approval"
    for target in (approval, gateway, auxiliary, args.module):
        if not target.is_file():
            raise RuntimeError(f"required gated-approval input is missing: {target}")

    module_hash = hashlib.sha256(args.module.read_bytes()).hexdigest()
    marker_content = f"commit={commit}\nmodule_sha256={module_hash}\n"
    if marker.exists():
        if marker.read_text() != marker_content:
            raise RuntimeError("installed gated-approval marker does not match this build")
        if not target_module.is_file() or target_module.read_bytes() != args.module.read_bytes():
            raise RuntimeError("installed gated-approval module does not match this build")
        patched_markers = {
            approval: "# --- Phase 2.5: conservative one-shot model gate ---",
            gateway: '"session_key_present": bool(_approval_session_key)',
            auxiliary: "# Preserve the Responses API service tier selected by security-",
        }
        for target, patched_marker in patched_markers.items():
            if target.read_text().count(patched_marker) != 1:
                raise RuntimeError(f"installed gated-approval patch drift in {target}")
        subprocess.run(
            [
                sys.executable,
                "-m",
                "py_compile",
                str(approval),
                str(gateway),
                str(auxiliary),
                str(target_module),
            ],
            check=True,
        )
        seed_config(args.config)
        return 0

    patch_approval(approval)
    patch_gateway(gateway)
    patch_auxiliary_client(auxiliary)
    shutil.copyfile(args.module, target_module)
    subprocess.run(
        [
            sys.executable,
            "-m",
            "py_compile",
            str(approval),
            str(gateway),
            str(auxiliary),
            str(target_module),
        ],
        check=True,
    )
    # Configuration is deliberately last: gated mode cannot be enabled unless
    # source anchors and compilation have already succeeded.
    marker.write_text(marker_content)
    seed_config(args.config)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
