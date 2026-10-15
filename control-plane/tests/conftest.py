"""Shared fixtures: in-memory fakes for Postgres session, Redis and Gemini."""

import hashlib
from datetime import UTC, datetime, timedelta

import httpx
import pytest
import pytest_asyncio
from sqlalchemy.exc import OperationalError

from app.config import Settings, get_settings
from app.deps import get_analyzer, get_redis, get_session
from app.main import create_app
from app.schemas import AnalyzeRequest, Diagnosis, Severity


class FakeSession:
    def __init__(self) -> None:
        self.added: list[object] = []
        self.committed = False
        self.rolled_back = False
        self.fail = False
        self._next_id = 1

    def add(self, obj: object) -> None:
        self.added.append(obj)

    def add_all(self, objs) -> None:
        self.added.extend(objs)

    async def flush(self) -> None:
        if self.fail:
            raise OperationalError("flush", {}, Exception("db down"))
        for o in self.added:
            if getattr(o, "id", None) is None:
                o.id = self._next_id
                self._next_id += 1

    async def commit(self) -> None:
        self.committed = True

    async def rollback(self) -> None:
        self.rolled_back = True

    async def execute(self, *_a, **_k):
        if self.fail:
            raise OperationalError("select", {}, Exception("db down"))


class FakeRedis:
    """Implements just enough of redis for the limiter (token bucket) and dedup."""

    def __init__(self) -> None:
        self.keys: dict[str, object] = {}
        self.buckets: dict[str, tuple[float, float]] = {}
        self.fail = False

    async def ping(self) -> bool:
        if self.fail:
            raise ConnectionError("redis down")
        return True

    async def set(self, name, value, nx=False, ex=None):
        if self.fail:
            raise ConnectionError("redis down")
        if nx and name in self.keys:
            return None
        self.keys[name] = value
        return True

    async def eval(self, script, numkeys, key, capacity, rate, now, cost):
        if self.fail:
            raise ConnectionError("redis down")
        tokens, ts = self.buckets.get(key, (float(capacity), now))
        tokens = min(float(capacity), tokens + max(0.0, now - ts) * float(rate))
        allowed = 0
        if tokens >= cost:
            tokens -= cost
            allowed = 1
        self.buckets[key] = (tokens, now)
        return [allowed, int(tokens)]


class FakeAnalyzer:
    def __init__(self) -> None:
        self.error: Exception | None = None
        self.calls: list[AnalyzeRequest] = []

    async def diagnose(self, req: AnalyzeRequest) -> Diagnosis:
        self.calls.append(req)
        if self.error:
            raise self.error
        return Diagnosis(
            root_cause="mocked gemini root cause",
            severity=Severity.HIGH,
            confidence=0.9,
            recommendations=["do the thing"],
        )


def event(kind: str = "cpu_spike", value: float = 95.0, **over) -> dict:
    base = {
        "kind": kind,
        "value": value,
        "threshold": 85.0,
        "timestamp": (datetime.now(UTC) - timedelta(seconds=1)).isoformat(),
    }
    return base | over


@pytest.fixture
def session() -> FakeSession:
    return FakeSession()


@pytest.fixture
def redis() -> FakeRedis:
    return FakeRedis()


@pytest.fixture
def analyzer() -> FakeAnalyzer:
    return FakeAnalyzer()


@pytest.fixture
def settings() -> Settings:
    return Settings(rate_limit_capacity=5, rate_limit_refill_per_sec=0.001, max_batch_events=500)


@pytest_asyncio.fixture
async def client(session, redis, analyzer, settings):
    app = create_app()
    app.dependency_overrides[get_session] = lambda: session
    app.dependency_overrides[get_redis] = lambda: redis
    app.dependency_overrides[get_analyzer] = lambda: analyzer
    app.dependency_overrides[get_settings] = lambda: settings
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://test") as c:
        yield c


def dedup_key(node: str, kind: str) -> str:
    return "dedup:" + hashlib.sha1(f"{node}|{kind}".encode()).hexdigest()  # noqa: S324
