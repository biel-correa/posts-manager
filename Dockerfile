# Stage 1: Build Go binary
FROM golang:alpine AS builder

ENV GOTOOLCHAIN=auto
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /linkedin-post-manager ./cmd/server

# Stage 2: Minimal Alpine runtime with timezone data & TLS roots
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget

WORKDIR /app
COPY --from=builder /linkedin-post-manager /app/linkedin-post-manager

EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/linkedin-post-manager"]
