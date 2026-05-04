"""Env-var configuration. Fail fast on startup if anything is missing."""

import os
from dataclasses import dataclass


@dataclass(frozen=True)
class Settings:
    database_url: str
    waitlist_api_key: str
    ip_salt: str
    cors_origins: tuple[str, ...]
    env: str  # "dev" | "prod"


def _required(name: str) -> str:
    val = os.environ.get(name)
    if not val:
        raise RuntimeError(f"missing required env var: {name}")
    return val


def load_settings() -> Settings:
    return Settings(
        database_url=_required("DATABASE_URL"),
        waitlist_api_key=_required("WAITLIST_API_KEY"),
        ip_salt=os.environ.get("IP_SALT", "change-me-please"),
        cors_origins=tuple(
            o.strip()
            for o in os.environ.get(
                "CORS_ORIGINS",
                "https://sypher.in,https://www.sypher.in",
            ).split(",")
            if o.strip()
        ),
        env=os.environ.get("ENV", "prod"),
    )


settings = load_settings()
