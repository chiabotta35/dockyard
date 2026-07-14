FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X github.com/dockyard/dockyard/internal/meta.Version=v0.1.0" -o /dockyard .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata docker-cli docker-cli-compose

COPY --from=builder /dockyard /usr/local/bin/dockyard

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -qO- http://localhost:8080/health || exit 1

ENTRYPOINT ["dockyard"]
CMD ["--web-ui", "--web-ui-port", "8080", "--schedule", "0 3 * * *", "--cleanup", "--update-on-start"]
