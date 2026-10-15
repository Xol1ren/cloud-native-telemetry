"""Redis-backed token-bucket rate limiting and anomaly de-duplication."""

import hashlib
import logging
import time
from typing import Protocol

logger = logging.getLogger(__name__)

# KEYS[1]=bucket key; ARGV: capacity, refill/sec, now (sec, float), cost
# Returns {allowed(0/1), remaining tokens}
TOKEN_BUCKET_LUA = """
local capacity = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local now = tonumber(ARGV[3])
local cost = tonumber(ARGV[4])
local data = redis.call('HMGET', KEYS[1], 'tokens', 'ts')
local tokens = tonumber(data[1])
local ts = tonumber(data[2])
if tokens == nil then tokens = capacity; ts = now end
tokens = math.min(capacity, tokens + math.max(0, now - ts) * rate)
local allowed = 0
if tokens >= cost then tokens = tokens - cost; allowed = 1 end
redis.call('HSET', KEYS[1], 'tokens', tokens, 'ts', now)
redis.call('EXPIRE', KEYS[1], math.ceil(capacity / rate) + 1)
return {allowed, math.floor(tokens)}
"""


class RedisLike(Protocol):
    async def eval(self, script: str, numkeys: int, *args: object) -> object: ...
    async def set(self, name: str, value: object, **kwargs: object) -> object: ...
    async def ping(self) -> object: ...


class RateLimiter:
    """Token bucket. Fails open if Redis is unavailable so ingestion keeps flowing."""

    def __init__(self, redis: RedisLike, capacity: int, refill_per_sec: float) -> None:
        self.redis = redis
        self.capacity = capacity
        self.refill = refill_per_sec

    async def allow(self, identity: str, cost: int = 1) -> bool:
        try:
            res = await self.redis.eval(
                TOKEN_BUCKET_LUA, 1, f"rl:{identity}", self.capacity, self.refill, time.time(), cost
            )
            return bool(int(res[0]))  # type: ignore[index]
        except Exception:
            logger.warning("rate limiter unavailable, failing open", exc_info=True)
            return True


class Deduplicator:
    """Suppresses repeats of the same (node, kind) within a TTL window."""

    def __init__(self, redis: RedisLike, ttl_seconds: int) -> None:
        self.redis = redis
        self.ttl = ttl_seconds

    @staticmethod
    def key(node: str, kind: str) -> str:
        return "dedup:" + hashlib.sha1(f"{node}|{kind}".encode()).hexdigest()  # noqa: S324

    async def is_new(self, node: str, kind: str) -> bool:
        try:
            return bool(await self.redis.set(self.key(node, kind), 1, nx=True, ex=self.ttl))
        except Exception:
            logger.warning("dedup unavailable, treating event as new", exc_info=True)
            return True
