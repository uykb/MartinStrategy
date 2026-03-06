# Build Stage
FROM golang:1.21-alpine AS builder

WORKDIR /app

# Dependencies
COPY go.mod go.sum ./
RUN go mod download

# Source
COPY . .

# Build
RUN go build -o bot cmd/bot/main.go

# Runtime Stage
FROM alpine:3.18

WORKDIR /app

# Install certificates for HTTPS
RUN apk --no-cache add ca-certificates

COPY --from=builder /app/bot .
COPY config.yaml .

CMD ["./bot"]
