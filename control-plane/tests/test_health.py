async def test_live(client):
    r = await client.get("/healthz/live")
    assert r.status_code == 200 and r.json()["status"] == "ok"


async def test_ready_ok(client):
    r = await client.get("/healthz/ready")
    assert r.status_code == 200
    assert r.json()["checks"] == {"postgres": "ok", "redis": "ok"}


async def test_ready_reports_dependency_failures(client, session, redis):
    session.fail = True
    redis.fail = True
    r = await client.get("/healthz/ready")
    assert r.status_code == 503
    checks = r.json()["checks"]
    assert checks["postgres"].startswith("error") and checks["redis"].startswith("error")


async def test_metrics_endpoint_exposes_latency_histogram(client):
    await client.get("/healthz/live")
    r = await client.get("/metrics")
    assert r.status_code == 200
    assert "cloudpulse_http_request_duration_seconds_bucket" in r.text
    assert 'path="/healthz/live"' in r.text
