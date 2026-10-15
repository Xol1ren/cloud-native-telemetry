"""Strict Pydantic v2 request/response schemas."""

from datetime import UTC, datetime
from enum import StrEnum

from pydantic import AwareDatetime, BaseModel, ConfigDict, Field, field_validator


class Strict(BaseModel):
    model_config = ConfigDict(extra="forbid", strict=False, str_strip_whitespace=True)


class AnomalyKind(StrEnum):
    CPU_SPIKE = "cpu_spike"
    MEMORY_PRESSURE = "memory_pressure"
    IO_WAIT = "io_wait"


class AnomalyEventIn(Strict):
    kind: AnomalyKind
    value: float = Field(ge=0, le=1e15)
    threshold: float = Field(ge=0, le=1e15)
    timestamp: AwareDatetime
    details: dict[str, float] = Field(default_factory=dict, max_length=32)

    @field_validator("timestamp")
    @classmethod
    def _not_far_future(cls, v: datetime) -> datetime:
        if (v - datetime.now(UTC)).total_seconds() > 300:
            raise ValueError("timestamp is more than 5 minutes in the future")
        return v


class IngestRequest(Strict):
    node: str = Field(min_length=1, max_length=253, pattern=r"^[A-Za-z0-9._-]+$")
    events: list[AnomalyEventIn] = Field(min_length=1, max_length=1000)


class IngestResponse(Strict):
    batch_id: int
    accepted: int
    deduplicated: int


class Severity(StrEnum):
    LOW = "low"
    MEDIUM = "medium"
    HIGH = "high"
    CRITICAL = "critical"


class Diagnosis(Strict):
    """Structured diagnosis; also used as the Gemini response schema."""

    root_cause: str = Field(min_length=1, max_length=2000)
    severity: Severity
    confidence: float = Field(ge=0, le=1)
    recommendations: list[str] = Field(min_length=1, max_length=10)


class AnalyzeRequest(Strict):
    node: str = Field(min_length=1, max_length=253, pattern=r"^[A-Za-z0-9._-]+$")
    events: list[AnomalyEventIn] = Field(min_length=1, max_length=50)
    context: str = Field(default="", max_length=2000)


class AnalyzeResponse(Strict):
    node: str
    source: str  # "gemini" or "fallback"
    diagnosis: Diagnosis
    degraded_reason: str | None = None


class HealthResponse(Strict):
    status: str
    checks: dict[str, str] = Field(default_factory=dict)
