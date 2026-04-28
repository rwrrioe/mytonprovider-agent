FROM golang:1.25-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o /app/bin/agent ./cmd/app

FROM alpine:3.21
WORKDIR /app
COPY --from=builder /app/bin/agent ./agent
COPY --from=builder /app/config ./config
EXPOSE 2112 16167/udp
ENTRYPOINT ["./agent"]
