"""POST /waitlist — captures email signups from sypher.in."""

import hashlib
from typing import Optional

from fastapi import APIRouter, Header, HTTPException, Request
from pydantic import BaseModel, EmailStr, Field

from app.config import settings
from app.db import acquire

router = APIRouter()


class WaitlistIn(BaseModel):
    email: EmailStr
    source: Optional[str] = Field(default=None, max_length=64)
    referrer: Optional[str] = Field(default=None, max_length=512)
    # Honeypot — real users don't fill this; bots scraping the form often will.
    hp: Optional[str] = Field(default=None, max_length=200)


class WaitlistOut(BaseModel):
    ok: bool
    new: bool


@router.post("/waitlist", response_model=WaitlistOut)
async def submit_waitlist(
    payload: WaitlistIn,
    request: Request,
    x_api_key: Optional[str] = Header(default=None, alias="X-API-Key"),
    user_agent: Optional[str] = Header(default=None),
) -> WaitlistOut:
    # Shared-secret check (Vercel route forwards this header)
    if x_api_key != settings.waitlist_api_key:
        raise HTTPException(status_code=401, detail="invalid api key")

    # Honeypot — pretend success silently so bots don't learn anything
    if payload.hp:
        return WaitlistOut(ok=True, new=False)

    email = payload.email.strip().lower()
    if not email or len(email) > 254:
        raise HTTPException(status_code=400, detail="invalid email")

    # Hash the IP with a salt — dedup signal without storing raw PII
    raw_ip = request.client.host if request.client else "0.0.0.0"
    ip_hash = hashlib.sha256(f"{settings.ip_salt}:{raw_ip}".encode()).hexdigest()

    ua = (user_agent or "")[:512] or None

    async with acquire() as conn:
        row = await conn.fetchrow(
            """
            INSERT INTO waitlist.signups (email, source, referrer, user_agent, ip_hash)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (lower(email)) DO NOTHING
            RETURNING id
            """,
            email,
            payload.source,
            payload.referrer,
            ua,
            ip_hash,
        )

    return WaitlistOut(ok=True, new=row is not None)
