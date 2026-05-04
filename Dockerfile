FROM python:3.12-slim-bookworm

# Runtime deps + non-root user
RUN apt-get update \
 && apt-get install -y --no-install-recommends curl \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* \
 && useradd --create-home --shell /bin/bash app

WORKDIR /app

# Install Python deps separately so the layer caches when source changes
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# App source
COPY app/ ./app/

# Migrations ship with the image. The container can run them via:
#   docker run --rm <image> migrate
COPY migrations/ ./migrations/

# Entry script — branches between `serve`, `migrate`, or arbitrary commands
COPY entrypoint.sh ./entrypoint.sh
RUN chmod +x ./entrypoint.sh

USER app

EXPOSE 8000

# Healthcheck — observable via `docker ps` STATUS
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -fs http://127.0.0.1:8000/health || exit 1

ENTRYPOINT ["./entrypoint.sh"]
CMD ["serve"]
