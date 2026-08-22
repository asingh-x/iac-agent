# ── Stage 1: Build React UI ──────────────────────────────────────────────────
FROM node:20-alpine AS ui-builder
WORKDIR /app/client
COPY client/package*.json ./
RUN npm ci --silent
COPY client/ ./
RUN npm run build

# ── Stage 2: Build Go server ──────────────────────────────────────────────────
FROM golang:1.26-alpine AS go-builder
WORKDIR /app
# Cache deps before copying source
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Copy built UI so it's available for go:embed if needed, and for the binary to serve
COPY --from=ui-builder /app/client/dist ./client/dist
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o tf-agent-server ./cmd/server

# ── Stage 3: Minimal runtime image ───────────────────────────────────────────
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
RUN addgroup -g 1000 tfagent && adduser -D -u 1000 -G tfagent tfagent
WORKDIR /app
RUN chown tfagent:tfagent /app

COPY --from=go-builder /app/tf-agent-server .
COPY --from=ui-builder /app/client/dist ./client/dist

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://localhost:8080/healthz || exit 1

USER tfagent
ENTRYPOINT ["./tf-agent-server"]
