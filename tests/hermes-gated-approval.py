#!/usr/bin/env python3
"""Regression tests for the vendored Hermes gated-approval extension."""

from __future__ import annotations

import ast
import importlib.util
import json
import math
import os
import sys
import types
import unittest
from pathlib import Path


sys.dont_write_bytecode = True
ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = Path(
    os.environ.get("HERMES_GATED_APPROVAL_MODULE", ROOT / "guest" / "hermes_gated_approval.py")
)
PATCHER_PATH = Path(
    os.environ.get(
        "HERMES_GATED_APPROVAL_PATCHER",
        ROOT / "guest" / "patch-hermes-gated-approval.py",
    )
)
SPEC = importlib.util.spec_from_file_location("gated_approval_under_test", MODULE_PATH)
gate = importlib.util.module_from_spec(SPEC)
assert SPEC and SPEC.loader
SPEC.loader.exec_module(gate)


class Message:
    def __init__(self, content):
        self.content = content


class Choice:
    def __init__(self, content):
        self.message = Message(content)


class Response:
    def __init__(self, verdict):
        self.choices = [Choice(json.dumps(verdict))]


BASE_GATE = {
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
}


def verdict(decision="approve", scope="once", risk="low", confidence=0.99, reason="safe"):
    return {
        "decision": decision,
        "scope": scope,
        "risk": risk,
        "confidence": confidence,
        "reason": reason,
    }


class TestGatedApproval(unittest.TestCase):
    def tearDown(self):
        gate._gate_context.set(None)

    def test_one_shot_and_codex_request_shape(self):
        calls = []

        def fake(**kwargs):
            calls.append(kwargs)
            return Response(verdict())

        first = gate.review_command("python -c 'print(1)'", "script", ["script"], BASE_GATE, call=fake)
        second = gate.review_command("python -c 'print(1)'", "script", ["script"], BASE_GATE, call=fake)
        self.assertEqual(first["decision"], "approve")
        self.assertEqual(second["decision"], "approve")
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0]["provider"], "openai-codex")
        self.assertEqual(calls[0]["model"], "gpt-5.5")
        self.assertEqual(calls[0]["extra_body"]["reasoning"]["effort"], "low")
        self.assertEqual(calls[0]["extra_body"]["service_tier"], "priority")
        self.assertEqual(calls[0]["timeout"], 30)

    def test_failures_escalate(self):
        malformed = gate.review_command("x", "y", [], BASE_GATE, call=lambda **_: "not json")
        self.assertEqual(malformed["decision"], "escalate")

        def failing(**_):
            raise TimeoutError("late")

        failed = gate.review_command("x", "y", [], BASE_GATE, call=failing)
        self.assertEqual(failed["decision"], "escalate")
        self.assertIn("TimeoutError", failed["reason"])

    def test_local_scope_risk_and_confidence_enforcement(self):
        for candidate in (
            verdict(scope="session"),
            verdict(risk="high"),
            verdict(confidence=0.74),
            verdict(risk="unknown"),
            verdict(confidence=math.nan),
            verdict(confidence=math.inf),
        ):
            result = gate.normalize_verdict(json.dumps(candidate), BASE_GATE)
            self.assertEqual(result["decision"], "escalate")

        weakened = dict(BASE_GATE, min_confidence=0, auto_approve_max_risk="critical")
        self.assertEqual(
            gate.normalize_verdict(json.dumps(verdict(risk="high")), weakened)["decision"],
            "escalate",
        )
        self.assertEqual(
            gate.normalize_verdict(json.dumps(verdict(confidence=0.74)), weakened)["decision"],
            "escalate",
        )

    def test_denial_is_preserved(self):
        result = gate.normalize_verdict(
            json.dumps(verdict(decision="deny", risk="critical", reason="catastrophic")),
            BASE_GATE,
        )
        self.assertEqual(result["decision"], "deny")

    def test_redaction_bounding_and_context_reset(self):
        token = gate.set_current_gate_context(
            {
                "api_key": "sk-secretvalue",
                "message": (
                    "FOO_API_KEY=supersecretvalue "
                    "SERVICE_TOKEN: abcdefghijklmnop "
                    "AWS_SECRET_ACCESS_KEY='quoted secret value' "
                    "AUTHORIZATION=Bearer bearercredentialvalue "
                    "PRIVATE_CREDENTIAL=credentialvalue "
                    "PUBLIC_URL=https://example.com"
                ),
            }
        )
        for index in range(15):
            gate.append_gate_tool_event({"index": index, "token": "secret"})
        packet = gate.build_review_packet("echo ok", "script", ["x"], 12000)
        self.assertNotIn("sk-secretvalue", packet)
        self.assertNotIn("supersecretvalue", packet)
        self.assertNotIn("abcdefghijklmnop", packet)
        self.assertNotIn("quoted secret value", packet)
        self.assertNotIn("bearercredentialvalue", packet)
        self.assertNotIn("credentialvalue", packet)
        self.assertIn("https://example.com", packet)
        self.assertEqual(packet.count('"index"'), 12)
        gate.reset_current_gate_context(token)
        self.assertIsNone(gate._gate_context.get())

    def test_oversized_exact_command_escalates_without_calling_reviewer(self):
        called = False

        def fake(**_):
            nonlocal called
            called = True
            return Response(verdict())

        result = gate.review_command(
            "x" * 2000,
            "warning",
            [],
            dict(BASE_GATE, max_context_chars=1000),
            call=fake,
        )
        self.assertEqual(result["decision"], "escalate")
        self.assertFalse(called)

    def test_none_dict_response_content_is_empty(self):
        self.assertEqual(gate._response_text({"content": None}), "")

    def test_dict_choices_response_content_is_extracted(self):
        response = {"choices": [{"message": {"content": "verdict"}}]}
        self.assertEqual(gate._response_text(response), "verdict")


class TestPatcherRecovery(unittest.TestCase):
    def test_marker_precedes_config_seeding_on_first_install(self):
        patcher = PATCHER_PATH.read_text()
        first_install = patcher.rsplit("    patch_approval(approval)\n", 1)[1]
        self.assertLess(
            first_install.index("marker.write_text(marker_content)"),
            first_install.index("seed_config(args.config)"),
        )


@unittest.skipUnless(os.environ.get("HERMES_GATED_APPROVAL_SOURCE"), "upstream source not requested")
class TestPatchedHermesIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        source = os.environ["HERMES_GATED_APPROVAL_SOURCE"]
        sys.path.insert(0, source)
        cls.approval = __import__("tools.approval", fromlist=["*"])
        cls.gated = __import__("tools.gated_approval", fromlist=["*"])

    def setUp(self):
        self.original_mode = self.approval._get_approval_mode
        self.original_config = self.approval._get_approval_config
        self.original_review = self.gated.review_command
        self.original_prompt = self.approval.prompt_dangerous_approval
        self.approval._get_approval_mode = lambda: "gated"
        self.approval._get_approval_config = lambda: {"mode": "gated", "gate": BASE_GATE}
        self.approval.prompt_dangerous_approval = lambda *args, **kwargs: "deny"
        os.environ["HERMES_INTERACTIVE"] = "1"

    def tearDown(self):
        self.approval._get_approval_mode = self.original_mode
        self.approval._get_approval_config = self.original_config
        self.gated.review_command = self.original_review
        self.approval.prompt_dangerous_approval = self.original_prompt
        os.environ.pop("HERMES_INTERACTIVE", None)

    def test_one_shot_denial_escalation_and_hardline_precedence(self):
        calls = []

        def approve(*args, **kwargs):
            calls.append(args[0])
            return verdict()

        self.gated.review_command = approve
        command = "python -c 'print(1)'"
        self.assertTrue(self.approval.check_all_command_guards(command, "local")["gate_approved"])
        self.assertTrue(self.approval.check_all_command_guards(command, "local")["gate_approved"])
        self.assertEqual(len(calls), 2)

        hardline = self.approval.check_all_command_guards("rm -rf /", "local")
        self.assertTrue(hardline["hardline"])
        self.assertEqual(len(calls), 2)

        self.gated.review_command = lambda *a, **k: verdict(
            decision="deny", risk="critical", reason="catastrophic"
        )
        denied = self.approval.check_all_command_guards(command, "local")
        self.assertTrue(denied["gate_denied"])
        self.assertFalse(denied["user_consent"])

        self.gated.review_command = lambda *a, **k: verdict(
            decision="escalate", risk="high", confidence=0.88, reason="needs human"
        )
        escalated = self.approval.check_all_command_guards(command, "local")
        self.assertFalse(escalated["approved"])
        self.assertIn("Gate verdict: escalate", escalated["description"])
        self.assertIn("needs human", escalated["description"])

    def test_codex_adapter_forwards_priority_low_and_timeout(self):
        auxiliary = __import__("agent.auxiliary_client", fromlist=["*"])
        runtime = __import__("agent.codex_runtime", fromlist=["*"])
        original_consume = runtime._consume_codex_event_stream
        captured = {}

        class Stream:
            def close(self):
                pass

        class Responses:
            def create(self, **kwargs):
                captured.update(kwargs)
                return Stream()

        class Client:
            responses = Responses()

        runtime._consume_codex_event_stream = lambda *a, **k: types.SimpleNamespace(
            output=[], usage=None
        )
        try:
            adapter = auxiliary._CodexCompletionsAdapter(Client(), "gpt-5.5")
            adapter.create(
                messages=[{"role": "user", "content": "review"}],
                timeout=30,
                extra_body={
                    "reasoning": {"effort": "low"},
                    "service_tier": "priority",
                },
            )
        finally:
            runtime._consume_codex_event_stream = original_consume
        self.assertEqual(captured["reasoning"]["effort"], "low")
        self.assertEqual(captured["service_tier"], "priority")
        self.assertEqual(captured["timeout"], 30)

    def test_gateway_tool_event_import_is_callback_local(self):
        gateway_path = Path(os.environ["HERMES_GATED_APPROVAL_SOURCE"]) / "gateway" / "run.py"
        tree = ast.parse(gateway_path.read_text())
        callbacks = [
            node
            for node in ast.walk(tree)
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            and node.name == "progress_callback"
        ]
        self.assertEqual(len(callbacks), 1)
        imports = [
            node
            for node in ast.walk(callbacks[0])
            if isinstance(node, ast.ImportFrom) and node.module == "tools.gated_approval"
        ]
        self.assertTrue(
            any(alias.name == "append_gate_tool_event" for node in imports for alias in node.names)
        )


if __name__ == "__main__":
    unittest.main(verbosity=2)
