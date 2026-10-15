"""AI root-cause analysis via Google Gemini with deterministic fallback."""

import asyncio
import json
import logging
from typing import Protocol

from app.schemas import AnalyzeRequest, Diagnosis, Severity

logger = logging.getLogger(__name__)

PROMPT_TEMPLATE = """You are a senior Site Reliability Engineer diagnosing a Linux host incident.

Node: {node}
Operator context: {context}

Anomaly events (most recent last):
{events}

Task:
1. Identify the single most likely root cause, correlating the events (e.g. high iowait
   together with memory pressure suggests swapping/page-cache thrash).
2. Rate severity as one of: low, medium, high, critical.
3. Give a confidence between 0 and 1.
4. Provide 2-5 concrete, ordered remediation steps with commands where useful.

Respond ONLY with JSON matching the provided schema. Do not speculate beyond the data."""


def build_prompt(req: AnalyzeRequest) -> str:
    lines = [
        f"- [{e.timestamp.isoformat()}] {e.kind.value}: value={e.value:.2f} "
        f"threshold={e.threshold:.2f} details={json.dumps(e.details, sort_keys=True)}"
        for e in sorted(req.events, key=lambda e: e.timestamp)
    ]
    return PROMPT_TEMPLATE.format(
        node=req.node, context=req.context or "none provided", events="\n".join(lines)
    )


class Analyzer(Protocol):
    async def diagnose(self, req: AnalyzeRequest) -> Diagnosis: ...


class GeminiAnalyzer:
    def __init__(self, api_key: str, model: str, timeout: float) -> None:
        self.api_key, self.model, self.timeout = api_key, model, timeout
        self._client = None

    def _get_client(self):  # lazy import keeps startup cheap and tests hermetic
        if self._client is None:
            from google import genai

            self._client = genai.Client(api_key=self.api_key)
        return self._client

    async def diagnose(self, req: AnalyzeRequest) -> Diagnosis:
        if not self.api_key:
            raise RuntimeError("GEMINI API key not configured")
        from google.genai import types

        client = self._get_client()
        resp = await asyncio.wait_for(
            client.aio.models.generate_content(
                model=self.model,
                contents=build_prompt(req),
                config=types.GenerateContentConfig(
                    response_mime_type="application/json",
                    response_schema=Diagnosis,
                    temperature=0.2,
                ),
            ),
            timeout=self.timeout,
        )
        return Diagnosis.model_validate_json(resp.text)


def fallback_diagnosis(req: AnalyzeRequest) -> Diagnosis:
    """Rule-based heuristics used when Gemini is unavailable or returns bad output."""
    kinds = {e.kind.value for e in req.events}
    recs: list[str] = []
    if {"memory_pressure", "io_wait"} <= kinds:
        cause = (
            "Memory pressure combined with high I/O wait indicates swapping or page-cache "
            "thrashing, with the disk becoming the bottleneck."
        )
        severity = Severity.CRITICAL
        recs = [
            "Identify top memory consumers: `ps aux --sort=-%mem | head`.",
            "Check swap activity: `vmstat 1 5` (si/so columns).",
            "Raise memory limits or scale out the workload.",
        ]
    elif "memory_pressure" in kinds:
        cause = (
            "Available memory is critically low; a process is likely leaking or over-allocating."
        )
        severity = Severity.HIGH
        recs = [
            "Inspect `smem -tk` / `ps aux --sort=-%mem`.",
            "Review OOM-killer logs: `dmesg -T | grep -i oom`.",
        ]
    elif "io_wait" in kinds:
        cause = "Processes are blocked on disk I/O; storage is saturated or degraded."
        severity = Severity.HIGH
        recs = ["Find heavy I/O processes: `iotop -oPa`.", "Check device latency: `iostat -x 1 5`."]
    else:
        cause = "Sustained CPU saturation, likely from a runaway or under-provisioned workload."
        severity = Severity.MEDIUM
        recs = [
            "Find hot processes: `top -o %CPU`.",
            "Consider horizontal scaling or CPU limits tuning.",
        ]
    return Diagnosis(root_cause=cause, severity=severity, confidence=0.4, recommendations=recs)
