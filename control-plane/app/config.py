"""Application settings loaded from environment variables."""

from functools import lru_cache

from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_prefix="CP_", env_file=".env", extra="ignore")

    database_url: str = "postgresql+asyncpg://cloudpulse:cloudpulse@localhost:5432/cloudpulse"
    redis_url: str = "redis://localhost:6379/0"

    gemini_api_key: str = ""
    gemini_model: str = "gemini-2.5-flash"
    gemini_timeout_seconds: float = 20.0

    rate_limit_capacity: int = 200  # bucket size (burst)
    rate_limit_refill_per_sec: float = 100.0
    dedup_ttl_seconds: int = 300

    max_batch_events: int = 500


@lru_cache
def get_settings() -> Settings:
    return Settings()
