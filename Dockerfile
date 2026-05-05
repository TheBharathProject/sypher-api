# ─── stage 1: build ─────────────────────────────────────────────────
# Pinned major-only on golang:* — Alpine is the smallest official base
# that ships glibc-free. Multi-stage means the final image doesn't carry
# the Go toolchain, source, or build cache.
FROM golang:1.25-alpine AS build

WORKDIR /src

# Cache deps separately from source
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY cmd     ./cmd
COPY internal ./internal

# Build static binaries. CGO_ENABLED=0 means no glibc dependency in the
# final image — we can use a tiny base. -ldflags "-s -w" strips debug
# symbols, shaving ~30% off the binary size.
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags="-s -w" -o /out/api      ./cmd/api      && \
    go build -trimpath -ldflags="-s -w" -o /out/migrate  ./cmd/migrate

# ─── stage 2: runtime ──────────────────────────────────────────────
FROM alpine:3.20

# curl is needed by HEALTHCHECK; ca-certificates so the binary can talk
# TLS to anything (Stripe, OpenAI, etc.)
RUN apk add --no-cache curl ca-certificates && \
    addgroup -S app && adduser -S app -G app

WORKDIR /app

# Copy compiled binaries
COPY --from=build /out/api      /usr/local/bin/api
COPY --from=build /out/migrate  /usr/local/bin/migrate

# Migrations ship inside the image — `migrate` reads /app/migrations
COPY migrations ./migrations

COPY entrypoint.sh ./entrypoint.sh
RUN chmod +x ./entrypoint.sh

USER app

EXPOSE 8000

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD curl -fs http://127.0.0.1:8000/health || exit 1

ENTRYPOINT ["./entrypoint.sh"]
CMD ["serve"]
