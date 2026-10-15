"""FastAPI dependency providers (overridable in tests)."""

from collections.abc import AsyncIterator

from fastapi import Depends, HTTPException, Request
from sqlalchemy.ext.asyncio import AsyncSession

from app.analysis import Analyzer
from app.config import Settings, get_settings
from app.redis_tools import Deduplicator, RateLimiter, RedisLike


async def get_session(request: Request) -> AsyncIterator[AsyncSession]:
    async with request.app.state.sessionmaker() as session:
        yield session


def get_redis(request: Request) -> RedisLike:
    return request.app.state.redis


def get_analyzer(request: Request) -> Analyzer:
    return request.app.state.analyzer


def get_deduplicator(
    redis: RedisLike = Depends(get_redis), settings: Settings = Depends(get_settings)
) -> Deduplicator:
    return Deduplicator(redis, settings.dedup_ttl_seconds)


async def enforce_rate_limit(
    request: Request,
    redis: RedisLike = Depends(get_redis),
    settings: Settings = Depends(get_settings),
) -> None:
    limiter = RateLimiter(redis, settings.rate_limit_capacity, settings.rate_limit_refill_per_sec)
    identity = request.client.host if request.client else "unknown"
    if not await limiter.allow(identity):
        raise HTTPException(
            status_code=429, detail="rate limit exceeded", headers={"Retry-After": "1"}
        )
