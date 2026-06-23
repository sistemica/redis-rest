# Build the application
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Download dependencies first to leverage layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the source and build a static binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o redis-rest-api .

# Minimal runtime image
FROM alpine:latest

RUN apk --no-cache add ca-certificates \
    && adduser -D -u 10001 appuser

WORKDIR /app
COPY --from=builder /app/redis-rest-api .

# Run as an unprivileged user
USER appuser

EXPOSE 8081

CMD ["./redis-rest-api"]
