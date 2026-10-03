# syntax=docker/dockerfile:1

# --- Admin UI -----------------------------------------------------------------
FROM node:22-alpine AS ui
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN mkdir -p ../internal/ui/dist && npm run build

# --- Go binary ----------------------------------------------------------------
FROM golang:1.27-alpine AS build
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/smtp-handler ./cmd/smtp-handler \
 && mkdir -p /out/data

# --- Runtime: distroless, non-root, no shell ----------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/smtp-handler /smtp-handler
COPY --from=build --chown=nonroot:nonroot /out/data /data

ENV SMTPH_ADDR=:8080 \
    SMTPH_ADMIN_ADDR=:9090 \
    SMTPH_STORE_DSN=/data/smtp-handler.db

VOLUME ["/data"]
EXPOSE 8080 9090
USER nonroot:nonroot
HEALTHCHECK --interval=15s --timeout=4s --start-period=10s --retries=3 \
  CMD ["/smtp-handler", "healthcheck"]
ENTRYPOINT ["/smtp-handler"]
CMD ["serve"]
