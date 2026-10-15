import pytest

from app.analysis import GeminiAnalyzer, build_prompt, fallback_diagnosis
from app.schemas import AnalyzeRequest
from tests.conftest import event


def req(*kinds: str) -> dict:
    return {"node": "node-1", "events": [event(k) for k in kinds], "context": "prod db host"}


async def test_analyze_uses_gemini(client, analyzer):
    r = await client.post("/api/v1/incidents/analyze", json=req("cpu_spike"))
    assert r.status_code == 200
    data = r.json()
    assert data["source"] == "gemini"
    assert data["diagnosis"]["root_cause"] == "mocked gemini root cause"
    assert data["degraded_reason"] is None
    assert len(analyzer.calls) == 1


@pytest.mark.parametrize(
    ("kinds", "severity"),
    [
        (["memory_pressure", "io_wait"], "critical"),
        (["memory_pressure"], "high"),
        (["io_wait"], "high"),
        (["cpu_spike"], "medium"),
    ],
)
async def test_analyze_falls_back_on_gemini_error(client, analyzer, kinds, severity):
    analyzer.error = TimeoutError("boom")
    r = await client.post("/api/v1/incidents/analyze", json=req(*kinds))
    assert r.status_code == 200
    data = r.json()
    assert data["source"] == "fallback"
    assert data["degraded_reason"] == "TimeoutError"
    assert data["diagnosis"]["severity"] == severity
    assert data["diagnosis"]["recommendations"]


async def test_analyze_validation(client):
    r = await client.post("/api/v1/incidents/analyze", json={"node": "n", "events": []})
    assert r.status_code == 422


def test_prompt_contains_events_and_context():
    parsed = AnalyzeRequest.model_validate(req("cpu_spike", "io_wait"))
    prompt = build_prompt(parsed)
    assert "node-1" in prompt and "prod db host" in prompt
    assert "cpu_spike" in prompt and "io_wait" in prompt
    assert "ONLY with JSON" in prompt


def test_fallback_diagnosis_is_valid():
    d = fallback_diagnosis(AnalyzeRequest.model_validate(req("cpu_spike")))
    assert 0 <= d.confidence <= 1


async def test_gemini_analyzer_without_key_raises():
    with pytest.raises(RuntimeError):
        await GeminiAnalyzer("", "m", 1).diagnose(AnalyzeRequest.model_validate(req("cpu_spike")))


async def test_gemini_analyzer_parses_structured_response(monkeypatch):
    class Resp:
        text = '{"root_cause":"x","severity":"low","confidence":0.5,"recommendations":["a"]}'

    class Models:
        async def generate_content(self, **kwargs):
            assert kwargs["config"].response_mime_type == "application/json"
            return Resp()

    class Aio:
        models = Models()

    class Client:
        aio = Aio()

    a = GeminiAnalyzer("key", "model", 5)
    a._client = Client()
    d = await a.diagnose(AnalyzeRequest.model_validate(req("cpu_spike")))
    assert d.root_cause == "x" and d.severity.value == "low"


async def test_gemini_malformed_output_triggers_fallback(client, analyzer):
    # Malformed model output raises a pydantic ValidationError inside diagnose.
    from pydantic import ValidationError

    from app.schemas import Diagnosis

    try:
        Diagnosis.model_validate_json("not json")
    except ValidationError as exc:
        analyzer.error = exc
    r = await client.post("/api/v1/incidents/analyze", json=req("cpu_spike"))
    assert r.json()["source"] == "fallback"
