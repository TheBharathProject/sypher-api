"""
One-shot migration runner. Connects to DATABASE_URL and applies every
.sql file in /app/migrations/ in lexicographic order. Each file must be
idempotent (CREATE ... IF NOT EXISTS, etc.) — re-running is safe.

Invoked by the container's entrypoint when started with the `migrate`
argument. See entrypoint.sh.
"""

from __future__ import annotations

import asyncio
import pathlib
import sys

import asyncpg

from app.config import settings


async def main() -> int:
    migrations_dir = pathlib.Path("/app/migrations")
    if not migrations_dir.exists():
        print(f"!!! no migrations directory at {migrations_dir}", file=sys.stderr)
        return 1

    files = sorted(migrations_dir.glob("*.sql"))
    if not files:
        print(">>> no migrations to apply")
        return 0

    conn = await asyncpg.connect(settings.database_url, command_timeout=30)
    try:
        for f in files:
            print(f">>> applying {f.name}")
            sql = f.read_text()
            await conn.execute(sql)
        print(f">>> done ({len(files)} migration(s) applied)")
    finally:
        await conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
