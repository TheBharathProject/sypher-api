FROM python:3.12-slim-bookworm

# System updates + minimal runtime deps. Non-root user for safety.
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --shell /bin/bash app

WORKDIR /app

# Install deps separately so docker layer caches dep install when source changes
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY app/ ./app/

USER app

# Container listens on 8000; host publishes to 127.0.0.1:8002 (not exposed publicly)
EXPOSE 8000

# Healthcheck — used by `docker ps` STATUS column
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -fs http://127.0.0.1:8000/health || exit 1

CMD ["uvicorn", "app.main:app", "--host", "0.0.0.0", "--port", "8000"]
