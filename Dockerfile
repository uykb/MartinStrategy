# Stage 1: Build Go Binary
FROM golang:1.21-alpine AS builder

WORKDIR /app
COPY go/ .
RUN go mod download
RUN go build -o bot main.go

# Stage 2: Python Runtime + Go Binary
FROM python:3.10-slim

WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Copy Go binary
COPY --from=builder /app/bot /usr/local/bin/go-bot

# Copy Python requirements and install
COPY python/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# Copy Python Code
COPY python/ .

# Env Vars
ENV PYTHONUNBUFFERED=1
ENV SYMBOL=HYPEUSDT

# Entrypoint
CMD ["python", "main.py"]
