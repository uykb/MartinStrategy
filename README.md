# MartinStrategy

A high-performance Martingale grid trading bot for **Binance Futures**, built with Go. Features an **event-driven finite state machine (ED-FSM)** architecture, a real-time web dashboard with K-line charts, session-based authentication, and a 10-level fixed-percentage asset allocation grid strategy.

## Features

- **Event-Driven Architecture**: Asynchronous message processing via EventBus decouples data sources from strategy logic.
- **Finite State Machine**: State transitions (`IDLE` → `WAITING_ENTRY` → `IN_POSITION` → `CLOSING`) prevent duplicate operations and race conditions.
- **Maker-Fee Entry**: Base order places a limit order at `currentPrice + 2*tickSize` (0.02% maker fee) with a 10-second fallback to market order.
- **Fixed-Percentage Asset Allocation Grid**:
  - Level 1 (Base Order): **6%** account USDT balance.
  - Levels 2–10 (Grid 1–9 Safety Orders): **3%, 3%, 5%, 5%, 18%, 32%, 56.7%, 57.8%, 116%** balance allocation.
  - Price intervals relative to previous order: **1.0%, 1.0%, 1.0%, 1.1%, 2.1%, 2.2%, 4.5%, 4.8%, 7.7%**.
- **Fixed Take Profit (TP)**: Set at `avgPrice × 1.008` (+0.80% above average entry price) with `ReduceOnly=true` protection.
- **Concurrency Safety**: `TryLock` guards, double-check flags, and failure rollback patterns prevent duplicate orders.
- **Self-Healing & Persistence**: Restores state and preserves open orders on restart when an existing position is detected.
- **Real-Time Web Dashboard**: Single-file embedded HTML dashboard with SSE push (2s interval), TradingView Lightweight Charts, and manual controls.
- **Docker-First Deployment**: Multi-stage lightweight Alpine container with Docker Compose support.

## Strategy Specification

### Asset Allocation & Grid Depths

| Level | Type | Balance Allocation % | Price Distance from Prev |
|-------|------|----------------------|--------------------------|
| 1 | Base Order | 6.0% | Maker Limit (`price + 2*tickSize`) |
| 2 | Safety Order 1 | 3.0% | -1.0% |
| 3 | Safety Order 2 | 3.0% | -1.0% |
| 4 | Safety Order 3 | 5.0% | -1.0% |
| 5 | Safety Order 4 | 5.0% | -1.1% |
| 6 | Safety Order 5 | 18.0% | -2.1% |
| 7 | Safety Order 6 | 32.0% | -2.2% |
| 8 | Safety Order 7 | 56.7% | -4.5% |
| 9 | Safety Order 8 | 57.8% | -4.8% |
| 10 | Safety Order 9 | 116.0% | -7.7% |

- **Minimum Order Value**: Enforced at 6.0 USDT per order.
- **Take Profit**: `avgPrice * 1.008` (full position close).

## Quick Start (Docker)

### 1. Configure Credentials

Edit `config.yaml`:

```yaml
exchange:
  api_key: "YOUR_BINANCE_API_KEY"
  api_secret: "YOUR_BINANCE_API_SECRET"
  symbol: "HYPEUSDT"
  use_testnet: false

strategy:
  max_safety_orders: 9
  base_ratio: 0.06

api:
  enabled: true
  port: 8080
  auth_token: "" # Set password for dashboard login, or leave empty for no auth

log:
  level: "info"
```

Or pass via environment variables (prefixed with `MARTIN_`):
```bash
export MARTIN_EXCHANGE_API_KEY="your_api_key"
export MARTIN_EXCHANGE_API_SECRET="your_api_secret"
export MARTIN_API_AUTH_TOKEN="your_dashboard_password"
```

### 2. Run with Docker Compose

```bash
docker compose up -d --build
```

### 3. Access Dashboard

Open your browser at `http://localhost:8080`.

To view logs:
```bash
docker compose logs -f
```

To stop:
```bash
docker compose down
```

## Architecture

```
┌─────────────────────┐      Events       ┌──────────────────┐
│   BinanceClient     │ ─────────────────▶│     EventBus     │
│   (Exchange Layer)  │  (TICK, ORDER)    │    (Core Layer)   │
│                     │                   │                  │
│ • REST API          │                   │ • Pub/Sub        │
│ • WebSocket Stream  │                   │ • Async Dispatch │
│ • Auto-Reconnect    │                   │ • Ring Buffer    │
│ • Price Polling     │                   └────────┬─────────┘
└─────────────────────┘                            │
                                          Subscribe │
                                                   ▼
                                          ┌──────────────────┐
                                          │ MartingaleStrategy│
                                          │  (Strategy/FSM)   │
                                          │                  │
                                          │ • State Machine   │
                                          │ • Grid Placement  │
                                          │ • TP Management   │
                                          │ • Position Monitor│
                                          └────────┬─────────┘
                                                   │
                                                   ▼
                                          ┌──────────────────┐
                                          │   Web Dashboard   │
                                          │   (API + SSE)     │
                                          │                  │
                                          │ • REST API        │
                                          │ • SSE Push (2s)   │
                                          │ • K-line Chart    │
                                          │ • Auth (Session)  │
                                          └──────────────────┘
```

## Repository Structure

```
.
├── cmd/bot/main.go              # Application entrypoint & graceful shutdown
├── internal/
│   ├── api/
│   │   ├── server.go            # HTTP server, REST endpoints, SSE stream, auth
│   │   └── dashboard.html       # Embedded single-file web dashboard
│   ├── config/config.go         # Viper configuration loader (YAML + ENV)
│   ├── core/event_bus.go        # Thread-safe event bus (Pub/Sub)
│   ├── exchange/binance.go      # Binance Futures REST & WebSocket client
│   ├── strategy/
│   │   ├── strategy.go          # Martingale finite state machine & order execution
│   │   └── dashboard.go         # Strategy state snapshots, cache & control methods
│   └── utils/
│       ├── precision.go         # StepSize and tickSize rounding helpers
│       └── logger.go            # Zap structured logger
├── config.yaml                  # Default runtime configuration
├── Dockerfile                   # Multi-stage production container build
├── docker-compose.yml           # Docker Compose deployment definition
├── go.mod / go.sum              # Go module dependencies
└── AGENTS.md                    # Operational & architectural guide
```
