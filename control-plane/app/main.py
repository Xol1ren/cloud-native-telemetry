"""Application factory, routes, metrics and lifespan."""

import asyncio
import logging
import time
from contextlib import asynccontextmanager

from fastapi import Depends, FastAPI, HTTPException, Request, Response
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Histogram, generate_latest
from redis.asyncio import Redis
from sqlalchemy import text
from sqlalchemy.exc import SQLAlchemyError
from sqlalchemy.ext.asyncio import AsyncSession

from app.analysis import Analyzer, GeminiAnalyzer, fallback_diagnosis
from app.config import Settings, get_settings
from app.db import AnomalyEvent, Base, TelemetryLog, make_engine, make_sessionmaker
from app.deps import (
    enforce_rate_limit,
    get_analyzer,
    get_deduplicator,
    get_redis,
    get_session,
)
from app.redis_tools import Deduplicator, RedisLike
from app.schemas import (
    AnalyzeRequest,
    AnalyzeResponse,
    HealthResponse,
    IngestRequest,
    IngestResponse,
)

logger = logging.getLogger("cloudpulse")

REQUEST_LATENCY = Histogram(
    "cloudpulse_http_request_duration_seconds",
    "HTTP request latency",
    ["method", "path", "status"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
)
INCIDENTS = Counter("cloudpulse_incidents_ingested_total", "Anomaly events stored", ["kind"])
DEDUPLICATED = Counter(
    "cloudpulse_incidents_deduplicated_total", "Anomaly events suppressed as duplicates"
)
ANALYSES = Counter("cloudpulse_analyses_total", "Incident analyses", ["source"])


async def _init_schema(engine, attempts: int = 10) -> None:
    for i in range(attempts):
        try:
            async with engine.begin() as conn:
                await conn.run_sync(Base.metadata.create_all)
            return
        except Exception as exc:  # DB may still be starting
            logger.warning("schema init attempt %d failed: %s", i + 1, exc)
            await asyncio.sleep(min(2**i * 0.2, 5))
    logger.error("could not initialise schema; /healthz/ready will report not ready")


@asynccontextmanager
async def lifespan(app: FastAPI):
    settings = get_settings()
    engine = make_engine(settings.database_url)
    app.state.sessionmaker = make_sessionmaker(engine)
    app.state.redis = Redis.from_url(settings.redis_url, decode_responses=True)
    app.state.analyzer = GeminiAnalyzer(
        settings.gemini_api_key, settings.gemini_model, settings.gemini_timeout_seconds
    )
    await _init_schema(engine)
    yield
    await app.state.redis.aclose()
    await engine.dispose()


def create_app() -> FastAPI:
    app = FastAPI(title="CloudPulse Control Plane", version="0.1.0", lifespan=lifespan)

    @app.middleware("http")
    async def timing(request: Request, call_next):
        start = time.perf_counter()
        status = 500
        try:
            response = await call_next(request)
            status = response.status_code
            return response
        finally:
            route = request.scope.get("route")
            path = route.path if route else "unmatched"
            REQUEST_LATENCY.labels(request.method, path, str(status)).observe(
                time.perf_counter() - start
            )

    @app.get("/metrics", include_in_schema=False)
    async def metrics() -> Response:
        return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)

    @app.get("/healthz/live", response_model=HealthResponse, tags=["health"])
    async def live() -> HealthResponse:
        return HealthResponse(status="ok")

    @app.get("/healthz/ready", response_model=HealthResponse, tags=["health"])
    async def ready(
        response: Response,
        session: AsyncSession = Depends(get_session),
        redis: RedisLike = Depends(get_redis),
    ) -> HealthResponse:
        checks: dict[str, str] = {}
        try:
            await session.execute(text("SELECT 1"))
            checks["postgres"] = "ok"
        except Exception as exc:
            checks["postgres"] = f"error: {type(exc).__name__}"
        try:
            await redis.ping()
            checks["redis"] = "ok"
        except Exception as exc:
            checks["redis"] = f"error: {type(exc).__name__}"
        ok = all(v == "ok" for v in checks.values())
        if not ok:
            response.status_code = 503
        return HealthResponse(status="ok" if ok else "unavailable", checks=checks)

    @app.post(
        "/api/v1/telemetry/ingest",
        response_model=IngestResponse,
        status_code=202,
        dependencies=[Depends(enforce_rate_limit)],
        tags=["telemetry"],
    )
    async def ingest(
        body: IngestRequest,
        session: AsyncSession = Depends(get_session),
        dedup: Deduplicator = Depends(get_deduplicator),
        settings: Settings = Depends(get_settings),
    ) -> IngestResponse:
        if len(body.events) > settings.max_batch_events:
            raise HTTPException(413, f"batch exceeds {settings.max_batch_events} events")
        fresh = [e for e in body.events if await dedup.is_new(body.node, e.kind.value)]
        duplicates = len(body.events) - len(fresh)
        try:
            log = TelemetryLog(
                node=body.node,
                event_count=len(body.events),
                duplicate_count=duplicates,
                payload=body.model_dump(mode="json"),
            )
            session.add(log)
            await session.flush()
            session.add_all(
                AnomalyEvent(
                    batch_id=log.id,
                    node=body.node,
                    kind=e.kind.value,
                    value=e.value,
                    threshold=e.threshold,
                    occurred_at=e.timestamp,
                    details=e.details,
                )
                for e in fresh
            )
            await session.commit()
        except SQLAlchemyError as exc:
            await session.rollback()
            logger.exception("database write failed")
            raise HTTPException(503, "database unavailable") from exc
        for e in fresh:
            INCIDENTS.labels(e.kind.value).inc()
        DEDUPLICATED.inc(duplicates)
        return IngestResponse(batch_id=log.id, accepted=len(fresh), deduplicated=duplicates)

    @app.post(
        "/api/v1/incidents/analyze",
        response_model=AnalyzeResponse,
        dependencies=[Depends(enforce_rate_limit)],
        tags=["incidents"],
    )
    async def analyze(
        body: AnalyzeRequest, analyzer: Analyzer = Depends(get_analyzer)
    ) -> AnalyzeResponse:
        try:
            diagnosis = await analyzer.diagnose(body)
            ANALYSES.labels("gemini").inc()
            return AnalyzeResponse(node=body.node, source="gemini", diagnosis=diagnosis)
        except Exception as exc:
            logger.warning("gemini analysis failed (%s); using fallback", type(exc).__name__)
            ANALYSES.labels("fallback").inc()
            return AnalyzeResponse(
                node=body.node,
                source="fallback",
                diagnosis=fallback_diagnosis(body),
                degraded_reason=type(exc).__name__,
            )

    return app


app = create_app()
