"""sypher-api — FastAPI entry point."""

import logging
from contextlib import asynccontextmanager

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from app.config import settings
from app.db import close_pool, init_pool
from app.routers import health, waitlist

log = logging.getLogger("sypher-api")
logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")


@asynccontextmanager
async def lifespan(app: FastAPI):
    log.info("startup: env=%s, cors=%s", settings.env, settings.cors_origins)
    await init_pool()
    yield
    await close_pool()
    log.info("shutdown complete")


app = FastAPI(
    title="sypher-api",
    version="0.1.0",
    description="Backend service for sypher.in. Hosts the waitlist + future tool read endpoints.",
    lifespan=lifespan,
    # Hide the OpenAPI docs in prod by default; flip ENV=dev locally to see them.
    docs_url="/docs" if settings.env == "dev" else None,
    redoc_url=None,
    openapi_url="/openapi.json" if settings.env == "dev" else None,
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=list(settings.cors_origins),
    allow_credentials=False,
    allow_methods=["GET", "POST"],
    allow_headers=["Content-Type", "X-API-Key"],
)

app.include_router(health.router, tags=["meta"])
app.include_router(waitlist.router, tags=["waitlist"])


@app.get("/", tags=["meta"])
async def root() -> dict:
    return {
        "service": "sypher-api",
        "version": "0.1.0",
        "status": "ok",
    }
