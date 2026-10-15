from app.db import AnomalyEvent, TelemetryLog
from tests.conftest import event


async def test_ingest_stores_batch_and_events(client, session):
    r = await client.post(
        "/api/v1/telemetry/ingest",
        json={"node": "node-1", "events": [event("cpu_spike"), event("io_wait", 40)]},
    )
    assert r.status_code == 202
    assert r.json() == {"batch_id": 1, "accepted": 2, "deduplicated": 0}
    assert session.committed
    logs = [o for o in session.added if isinstance(o, TelemetryLog)]
    anomalies = [o for o in session.added if isinstance(o, AnomalyEvent)]
    assert len(logs) == 1 and logs[0].event_count == 2
    assert {a.kind for a in anomalies} == {"cpu_spike", "io_wait"}
    assert all(a.batch_id == 1 and a.node == "node-1" for a in anomalies)


async def test_duplicates_are_suppressed(client, session):
    body = {"node": "node-1", "events": [event("cpu_spike")]}
    first = await client.post("/api/v1/telemetry/ingest", json=body)
    second = await client.post("/api/v1/telemetry/ingest", json=body)
    assert first.json()["accepted"] == 1
    assert second.json()["accepted"] == 0 and second.json()["deduplicated"] == 1
    assert len([o for o in session.added if isinstance(o, AnomalyEvent)]) == 1
    # The same kind on a different node is not a duplicate.
    third = await client.post("/api/v1/telemetry/ingest", json=body | {"node": "node-2"})
    assert third.json()["accepted"] == 1


async def test_validation_rejects_bad_payloads(client, settings):
    settings.rate_limit_capacity = 100
    bad_bodies = [
        {"node": "n", "events": []},
        {"node": "bad node!", "events": [event()]},
        {"node": "n", "events": [event(kind="nope")]},
        {"node": "n", "events": [event(value=-1)]},
        {"node": "n", "events": [event(timestamp="2020-01-01T00:00:00")]},  # naive datetime
        {"node": "n", "events": [event()], "unexpected": 1},
    ]
    for body in bad_bodies:
        r = await client.post("/api/v1/telemetry/ingest", json=body)
        assert r.status_code == 422, body


async def test_rejects_far_future_timestamp(client):
    r = await client.post(
        "/api/v1/telemetry/ingest",
        json={"node": "n", "events": [event(timestamp="2999-01-01T00:00:00+00:00")]},
    )
    assert r.status_code == 422


async def test_rate_limit_returns_429(client):
    body = {"node": "n", "events": [event()]}
    codes = [
        (await client.post("/api/v1/telemetry/ingest", json=body)).status_code for _ in range(7)
    ]
    assert codes[:5] == [202] * 5
    assert codes[5:] == [429, 429]


async def test_rate_limit_fails_open_when_redis_down(client, redis):
    redis.fail = True
    r = await client.post("/api/v1/telemetry/ingest", json={"node": "n", "events": [event()]})
    assert r.status_code == 202


async def test_database_failure_returns_503(client, session):
    session.fail = True
    r = await client.post("/api/v1/telemetry/ingest", json={"node": "n", "events": [event()]})
    assert r.status_code == 503
    assert session.rolled_back


async def test_batch_size_limit(client, settings):
    settings.max_batch_events = 1
    r = await client.post(
        "/api/v1/telemetry/ingest",
        json={"node": "n", "events": [event("cpu_spike"), event("io_wait")]},
    )
    assert r.status_code == 413
