# Martin

A high-performance Martingale grid trading bot for **Binance Futures**, built with Go. Features an **event-driven finite state machine (ED-FSM)** architecture, a real-time web dashboard with K-line charts, session-based authentication, and a fixed-percentage grid strategy optimized for maker fee savings.

## Features

- **Event-Driven Architecture**: Async message processing via EventBus decouples data sources from strategy logic
- **Finite State Machine**: Clear state transitions (IDLE → WAITING_ENTRY → IN_POSITION) prevent logic conflicts
- **Maker-Fee Entry**: Base orders use limit orders at +2 tick from market price, with a 10-second timeout fallback to market order — saves ~0.03% per entry
- **9-Level Fixed Percentage Grid**: Grid safety orders placed at pre-defined percentage intervals (1.0%–7.7% relative depth), no ATR dependency
- **Fibonacci Quantity Scaling**: Safety order sizes follow Fibonacci sequence scaled by 0.5: 0.5, 0.5, 1, 1.5, 2.5, 4, 6.5, 10.5, 17×
- **VWAP + 0.80% Take Profit**: TP calculated as VWAP × 1.008 using 15-minute candle data from the last 24 hours
- **ReduceOnly Protection**: TP orders use Binance's ReduceOnly flag to prevent accidental short positions
- **Concurrency Safety**: TryLock guards, double-check flags, and failure rollback patterns prevent duplicate orders
- **Self-Healing**: Automatic position state sync on startup; WebSocket reconnect with exponential backoff
- **State Recovery**: On restart, preserves open orders if a position exists (across restarts)
- **Web Dashboard**: Embedded single-file HTML dashboard with SSE real-time updates, K-line chart, grid visualization, and manual controls
- **Session-Based Auth**: 24-hour expiring HMAC-signed session tokens, logged-out automatically on expiry
- **Structured Logging**: Zap-based JSON logging for production observability
- **Graceful Shutdown**: SIGINT/SIGTERM handler for clean teardown

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
                                            │ • SSE Push        │
                                            │ • K-line Chart    │
                                            │ • Auth (Session)  │
                                            └──────────────────┘
```

## Directory Structure

```
.
├── cmd/
│   └── bot/
│       └── main.go              # Entry point, lifecycle management
├── internal/
│   ├── api/
│   │   ├── server.go            # HTTP server, SSE, auth, routes
│   │   └── dashboard.html       # Embedded single-file web dashboard
│   ├── config/
│   │   └── config.go            # Viper config (YAML + env vars)
│   ├── core/
│   │   └── event_bus.go         # Event bus (pub/sub pattern)
│   ├── exchange/
│   │   └── binance.go           # Binance Futures client (REST + WebSocket)
│   ├── strategy/
│   │   ├── strategy.go          # Martingale strategy FSM
│   │   └── dashboard.go         # Snapshot DTOs, control methods, cache
│   └── utils/
│       ├── indicators.go        # Price/quantity precision helpers
│       └── logger.go            # Zap structured logger
├── config.yaml                  # Default configuration
├── Dockerfile                   # Multi-stage Docker build
├── docker-compose.yml           # Docker Compose
├── go.mod / go.sum              # Go module dependencies
├── README.md
└── AGENTS.md                    # Developer/Agent guide
```

## Quick Start

### Docker Compose (Recommended)

```bash
# 1. Edit config.yaml with your API credentials
vim config.yaml

# 2. Start
docker-compose up -d

# 3. View logs
docker-compose logs -f
```

### Local Development

```bash
# 1. Install dependencies
go mod tidy

# 2. Edit config
vim config.yaml

# 3. Run
go run cmd/bot/main.go

# 4. Open dashboard
# http://localhost:8080 (if MARTIN_API_AUTH_TOKEN is set, you'll need to log in)
```

### Build Binary

```bash
go build -o bot cmd/bot/main.go
./bot
```

## Configuration

### config.yaml

```yaml
exchange:
  api_key: ""              # Binance API Key
  api_secret: ""           # Binance API Secret
  symbol: "HYPEUSDT"       # Trading pair
  use_testnet: false       # Use Binance testnet

strategy:
  max_safety_orders: 9     # Maximum grid levels
  base_ratio: 0.05         # Base order = balance × base_ratio

api:
  enabled: true            # Enable web dashboard
  port: 8080               # Dashboard port
  auth_token: ""           # Auth token (leave empty for no auth)

log:
  level: "info"            # Log level: debug, info, warn, error
```

### Environment Variables

All config fields can be overridden with `MARTIN_`-prefixed env vars:

```bash
export MARTIN_EXCHANGE_API_KEY="your_api_key"
export MARTIN_EXCHANGE_API_SECRET="your_api_secret"
export MARTIN_EXCHANGE_SYMBOL="HYPEUSDT"
export MARTIN_EXCHANGE_USE_TESTNET="false"
export MARTIN_STRATEGY_MAX_SAFETY_ORDERS="9"
export MARTIN_STRATEGY_BASE_RATIO="0.05"
export MARTIN_API_ENABLED="true"
export MARTIN_API_PORT="8080"
export MARTIN_API_AUTH_TOKEN="your_auth_token"
export MARTIN_LOG_LEVEL="info"
```

## Strategy Details

### State Machine

```
┌──────────┐    Tick Event     ┌──────────────────┐
│  IDLE    │ ────────────────▶│  WAITING_ENTRY    │
│ (No Pos) │                   │ (Limit Order Sent)│
└──────────┘                   └────────┬─────────┘
     ▲                                  │ Order Fills
     │                                  ▼
     │                         ┌──────────────────┐
     │    TP Filled (SELL)     │  IN_POSITION      │
     └────────────────────────│  (Holding Position)│
                               │                   │
                               │ Safety Order Fill │
                               │     → Update TP   │
                               └──────────────────┘
```

| State | Description | Entry Trigger | Exit Trigger |
|-------|-------------|---------------|-------------|
| `IDLE` | No position, waiting for tick | Startup / TP fill / manual close | Tick received |
| `WAITING_ENTRY` | Base limit order placed, waiting for fill | `enterLong()` | Order fills |
| `IN_POSITION` | Position active, grid orders placed | Base order fills | TP fill / manual close |
| `PLACING_GRID` | (Transient) Grid orders being placed | Order fill detected | Grid placed |

### Trading Flow

1. **Entry (`enterLong`)**: IDLE state receives a tick event → fetches USDT balance → calculates base quantity = `balance × base_ratio` → places a **limit order** at `currentPrice + 2×tickSize` (maker fee 0.02%) → sets state to `WAITING_ENTRY`
2. **Timeout Fallback**: If the limit order doesn't fill within 10 seconds, cancels it and falls back to a **market order** (taker fee 0.05%)
3. **Grid Placement (`placeGridOrders`)**: After entry fills, places 9 limit buy safety orders at fixed percentage intervals below entry price, using Fibonacci-scaled quantities
4. **TP Management (`updateTP`)**: Calculates VWAP from 15-minute candles (24h window), sets TP at `VWAP × 1.008`. Updated after each safety order fill
5. **Exit**: When TP fills, cancels all remaining orders, resets state to IDLE

### Grid Levels

Prices are calculated relative to the previous level:

| Level | Depth from Previous | Fibonacci Multiplier |
|-------|---------------------|---------------------|
| L0 (Base) | — | — |
| L1 | -1.0% | 0.5× |
| L2 | -1.0% | 0.5× |
| L3 | -1.0% | 1× |
| L4 | -1.1% | 1.5× |
| L5 | -2.1% | 2.5× |
| L6 | -2.2% | 4× |
| L7 | -4.5% | 6.5× |
| L8 | -4.8% | 10.5× |
| L9 | -7.7% | 17× |

- L9 price ≈ 0.771 × entry price (approximately -22.9% max depth)
- Base quantity (unit) = `minNotional / entryPrice`, where `minNotional = balance × base_ratio`
- Safety order quantity = `unitQty × FibonacciMultiplier`
- Minimum order value enforced at 6 USDT

### Take Profit

- **Calculation**: `TP = VWAP × 1.008` (VWAP from last 24h of 15-minute candles)
- **Quantity**: Full position close
- **Update**: Recalculated after each safety order fill
- **Protection**: TP orders use `ReduceOnly=true` to prevent accidental short positions

## Web Dashboard

The embedded dashboard provides real-time trading visibility:

| Feature | Description |
|---------|-------------|
| **Real-time State** | SSE push of balance, position, orders, fills, alerts (5s refresh) |
| **K-Line Chart** | TradingView Lightweight Charts with candlestick + volume. Intervals: 15m, 1h, 4h, 1d, 1w, 1M |
| **Price Lines** | Entry price (blue dashed), TP price + qty (green solid), grid safety orders + qty (amber dotted) |
| **Controls** | Pause/Resume (toggle), Close All (two-step confirmation), Refresh TP |
| **Connection Info** | Server public IP display (copy to clipboard) for Binance API whitelist |
| **Auth** | Session-based login with 24h auto-expiry |

### API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | GET | Dashboard HTML |
| `/api/health` | GET | Server health + public IP |
| `/api/state` | GET | Current strategy snapshot |
| `/api/stream` | GET | SSE event stream (real-time push) |
| `/api/login` | POST | Authenticate, receive session token |
| `/api/pause` | POST | Pause strategy (blocks new entries) |
| `/api/resume` | POST | Resume strategy |
| `/api/close-all` | POST | Emergency close: cancel all orders + market exit |
| `/api/klines` | GET | OHLCV data for chart (`?interval=1h&limit=200`) |

All endpoints (except `/api/health`, `/api/login`, `/`, `/api/stream`) require `Authorization: Bearer <session_token>` header.

### Authentication Flow

```
User enters password
         │
         ▼
POST /api/login  {"token": "password"}
         │
         ▼
Server generates HMAC-signed session token (24h expiry)
         │
         ▼
Browser stores session token in localStorage
         │
         ▼
All requests → Authorization: Bearer <session_token>
         │
         └── Expired (24h) → 401 → Auto-redirect to login
```

## Concurrency Safety

### 1. TryLock Guards

```go
// Grid placement (blocking-safe)
if !s.gridMu.TryLock() {
    s.gridSkipCount++  // monitoring counter
    return
}
defer s.gridMu.Unlock()

// TP update (blocking-safe)
if !s.tpMu.TryLock() {
    s.tpSkipCount++
    return
}
defer s.tpMu.Unlock()
```

### 2. Double-Check Flags

```go
// Fast path: lock-free check
s.mu.RLock()
if s.gridPlaced { s.mu.RUnlock(); return }
s.mu.RUnlock()

// Lock path: re-check after acquiring lock
s.gridMu.Lock()  // or TryLock
s.mu.RLock()
if s.gridPlaced { ... }  // protected against races
s.mu.RUnlock()
```

### 3. Failure Rollback

```go
s.mu.Lock()
s.currentState = StateWaitingEntry
s.mu.Unlock()

if err := s.enterLong(price); err != nil {
    s.mu.Lock()
    s.currentState = StateIdle  // Rollback on failure
    s.mu.Unlock()
}
```

## Dependencies

| Component | Library | Version |
|-----------|---------|---------|
| Language | Go | 1.25+ |
| Exchange API | go-binance (futures) | v2.8.11 |
| Configuration | Viper | v1.21.0 |
| Logging | Zap | v1.27.1 |
| Charting | Lightweight Charts (CDN) | v4.2.0 |

## Development

```bash
# Build
go build -o bot cmd/bot/main.go

# Run
go run cmd/bot/main.go

# Test
go test ./...

# Code checks
go vet ./...
go fmt ./...

# Dependencies
go mod tidy
go mod download

# Docker
docker-compose build
docker-compose up -d
```

## Key Constants

| Constant | Value | Description |
|----------|-------|-------------|
| `MinOrderValue` | 6.0 USDT | Minimum order value for Binance Futures |
| `EntryTimeout` | 10s | Timeout for limit order → market fallback |
| Price polling | 10s | Tick event interval |
| Position monitor | 5s | Manual close detection interval |
| Grid order spacing | 200ms | API rate limit protection |
| Session duration | 24h | Auth session lifetime |
| User stream keepalive | 15min | Binance WebSocket heartbeat |
| Server time sync | 5min | Periodic clock sync |
| WS reconnect retries | 5 | Exponential backoff |
| Event queue buffer | 1000 | Max pending events |

## Log Examples

```json
{"level":"info","msg":"Starting MartinStrategy Bot","symbol":"HYPEUSDT"}
{"level":"info","msg":"Server time synced","offset_ms":110}
{"level":"info","msg":"容器公网 IP（请加入 Binance API 白名单）","ip":"51.195.62.118"}
{"level":"info","msg":"Dynamic MinNotional","balance":1000,"ratio":0.05,"notional":50}
{"level":"info","msg":"Tick received","price":39.638,"state":"IDLE","gridPlaced":false}
{"level":"info","msg":"Placing Safety Order","index":1,"price":39.24,"qty":1.5,"dist_pct":1.0}
{"level":"info","msg":"VWAP calculated","vwap":39.15,"bars":96}
{"level":"warn","msg":"placeGridOrders skipped: already running","skip_count":5}
```

## Risk Disclaimer

- Martingale strategies carry **extreme risk** in prolonged bearish conditions
- Always test with `use_testnet: true` before real funds
- Ensure your Binance API key has Futures trading permissions and whitelist the server IP
- This software is for educational purposes. Use at your own risk.

## License

MIT License
