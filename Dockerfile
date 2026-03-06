# Build Stage
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Dependencies
# Install build tools for CGO (needed for go-sqlite3)
RUN apk add --no-cache gcc musl-dev

COPY go.mod go.sum ./
RUN go mod download

# Source
COPY . .

# Build
# Enable CGO for sqlite3
ENV CGO_ENABLED=1
RUN go build -o bot cmd/bot/main.go

# Runtime Stage
FROM alpine:3.18

WORKDIR /app

# Install certificates for HTTPS and sqlite libs if needed (though static build usually bundles it)
# sqlite3 dynamic lib might be needed if CGO linked dynamically
RUN apk --no-cache add ca-certificates sqlite-libs

COPY --from=builder /app/bot .
COPY config.yaml .

CMD ["./bot"]
