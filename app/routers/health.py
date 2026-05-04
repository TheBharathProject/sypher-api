from fastapi import APIRouter, HTTPException

from app.db import acquire

router = APIRouter()


@router.get("/health")
async def health() -> dict:
    """Liveness + DB reachability check."""
    try:
        async with acquire() as conn:
            await conn.execute("SELECT 1")
    except Exception as exc:  # pragma: no cover
        raise HTTPException(status_code=503, detail=f"db unreachable: {exc}")
    return {"ok": True}
