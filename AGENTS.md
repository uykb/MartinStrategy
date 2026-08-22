# AGENTS.md

> **Source of truth**: `README.md` is partly stale. It still claims Fibonacci quantity
> multipliers, VWAP-based TP (`VWAP × 1.008`), and a 5s dashboard SSE push. The actual
> code uses **fixed multipliers** (`safetyOrderMultipliers`), **`avgPrice × 1.008`** TP
> (`internal/strategy/strategy.go:807`), and a **2s** SSE push (`internal/api/server.go:300`).
> Trust this file and the code over README.

## Build / Test / Lint Commands

```bash
# Build binary
go build -o bot cmd/bot/main.go

# Run all tests
go test ./...

# Run single test (example pattern)
go test -run TestFunctionName ./internal/utils/...
go test -v -run TestCalculateATR ./internal/utils/

# Code checks
go vet ./...
go fmt ./...

# Dependencies
go mod tidy
go mod download

# Run locally
go run cmd/bot/main.go

# Docker (v2 CLI)
docker compose build
docker compose up -d
```

## Code Style Guidelines

### Imports
- Group imports: stdlib, blank line, third-party, blank line, project packages
- Use `goimports` style (project uses module `github.com/uykb/MartinStrategy`)
- Example:
```go
import (
    "context"
    "fmt"
    "sync"

    "github.com/adshao/go-binance/v2/futures"
    "go.uber.org/zap"

    "github.com/uykb/MartinStrategy/internal/config"
    "github.com/uykb/MartinStrategy/internal/core"
)
```

### Formatting
- Standard Go formatting (`gofmt`)
- Line length: aim for ~100 chars, no hard limit
- Comments for exported types/functions start with the name

### Types
- Use custom type definitions for states/enums: `type State string`, `type EventType string`
- Prefer explicit types over primitives for domain concepts
- Struct tags use `mapstructure` for config, `json` for API/SSE models

### Naming Conventions
- **Exported**: PascalCase (e.g., `EventBus`, `NewBinanceClient`)
- **Unexported**: camelCase (e.g., `currentState`, `handleTick`)
- **Constants**: PascalCase for exported, camelCase for unexported (e.g., `StateIdle`, `minNotional`)
- **Interfaces**: `-er` suffix (e.g., `EventHandler`)
- **Acronyms**: Keep uppercase (e.g., `ATR`, `TP`, `API`)
- Event type constants: `Event` prefix (e.g., `EventTick`, `EventOrderUpdate`)

### Error Handling
- Wrap errors with context: `fmt.Errorf("failed to get exchange info: %w", err)`
- Return errors to callers; only log at appropriate levels
- Fatal only in `main.go` or initialization failures
- Use Zap for structured logging with fields:
```go
utils.Logger.Error("Failed to do something", zap.Error(err), zap.String("symbol", symbol))
```

### Concurrency Patterns
- Always use `sync.Mutex` or `sync.RWMutex` for shared state
- Use `TryLock()` pattern for re-entrant prevention:
```go
if !s.gridMu.TryLock() {
    s.gridSkipCount++
    return
}
defer s.gridMu.Unlock()
```
- Keep network calls OUTSIDE of locks to prevent blocking
- Rollback state on failure:
```go
s.mu.Lock()
s.currentState = StatePlacingGrid
s.mu.Unlock()

if err := doNetworkCall(); err != nil {
    s.mu.Lock()
    s.currentState = StateIdle  // Rollback
    s.mu.Unlock()
}
```

### Configuration
- `config.yaml` is **required at startup**: `main.go` calls `LoadConfig("config.yaml")` and
  panics if the file is missing. Env vars only *override* fields; they cannot replace the file.
- Environment variables use `MARTIN_` prefix (e.g., `MARTIN_EXCHANGE_API_KEY`)
- Struct field tags use snake_case: `mapstructure:"api_key"`
- YAML config file uses snake_case keys

### Comments
- All exported items must have a comment starting with the name
- Comments in Chinese are acceptable (existing code has some)
- Doc comments should explain purpose, not implementation details

## Architecture Quick Reference

| Package | Purpose |
|---------|---------|
| `internal/config` | Viper-based config loading from YAML/env |
| `internal/core` | Event bus with Pub/Sub pattern |
| `internal/exchange` | Binance Futures WebSocket + REST client |
| `internal/strategy` | Martingale FSM + web dashboard state |
| `internal/api` | Web dashboard HTTP server with SSE push |
| `internal/utils` | Rounding helpers (ToFixed, RoundToTickSize), Zap logger |

## Key Constants
- `MinOrderValue = 6.0` - Minimum USDT order value for Binance Futures (动态头仓下限)
- `EntryTimeout = 10 * time.Second` - 首仓限价单超时，超后回退市价
- `TPCooldown = 30 * time.Second` - 止盈成交后冷却期，防止反复开平仓
- Event queue buffer: 1000
- Grid levels: 9 max (fixed percentage grid, balance asset allocation sizing)
- Safety order balance allocation ratios: 3%, 3%, 5%, 5%, 18%, 32%, 56.7%, 57.8%, 116% (for levels 2-10 / Grids 1-9)
- Price polling interval: 10s
- Position monitor interval: 5s
- Grid order API rate limit: 200ms between orders
- User stream keepalive: 15m (Binance requires within 60m)
- Server time sync: every 5m
- WebSocket reconnect: up to 5 retries with exponential backoff

## Key Config Parameters

```yaml
exchange:
  api_key: ""
  api_secret: ""
  symbol: "HYPEUSDT"
  use_testnet: false

strategy:
  max_safety_orders: 9    # 最大网格层数
  base_ratio: 0.06        # 头仓金额 = 账户 USDT 余额 × base_ratio（6% 固定比例）

api:
  enabled: true
  port: 8080              # Web dashboard HTTP port
  auth_token: ""          # Dashboard auth (empty = no auth)

log:
  level: "info"
```

## Grid Strategy Details

### Grid Distances — Fixed Percentage (9 Levels)

| Level | Distance (%) | Description |
|-------|-------------|-------------|
| 1 | 1.0% | 首层保护 |
| 2 | 1.0% | 第二层保护 |
| 3 | 1.0% | 中短期保护 |
| 4 | 1.1% | 中期保护 |
| 5 | 2.1% | 中长期保护 |
| 6 | 2.2% | 长期保护 |
| 7 | 4.5% | 长期保护 |
| 8 | 4.8% | 更深层保护 |
| 9 | 7.7% | 最深层保护 |

- Distances are **relative to previous order**, not absolute
- Beyond level 9, no further orders are placed (`max_safety_orders` limit)

### Fixed Safety Order Balance Allocations

```go
var safetyOrderAllocations = []float64{0.03, 0.03, 0.05, 0.05, 0.18, 0.32, 0.567, 0.578, 1.16}
```

Each safety order notional = `balance × safetyOrderAllocations[i]`, and quantity = `orderNotional / gridPrice`.

### Entry Order

- First position is a **maker limit** order priced at `currentPrice + 2*tickSize`
  (`internal/strategy/strategy.go:505`), not a market order, to capture maker fees.
- If unfilled within `EntryTimeout` (10s), it is cancelled and replaced with a market order.
- `BaseRatio` default is `0.06` (see `config.yaml`); README's `0.05` example is outdated.

### Take Profit (TP)

- TP price: `avgPrice * 1.008` (fixed 0.80% above average entry price)
- TP quantity: full position close
- Updated after each safety order fill
- Old TP is cancelled before new TP is placed
- Uses `tpMu.TryLock()` to prevent concurrent TP updates

## Dynamic Notional Calculation

头仓金额通过 `calcMinNotional()` 动态计算：

```go
func (s *MartingaleStrategy) calcMinNotional() float64 {
    balance, err := s.exchange.GetBalance()  // REST API 查询 USDT 余额
    if err != nil {
        return MinOrderValue  // 降级到 6.0 USDT
    }
    notional := balance * s.cfg.BaseRatio  // 余额 × 比例
    if notional < MinOrderValue {
        notional = MinOrderValue  // 不低于 Binance 最低限制
    }
    return notional
}
```

- 调用时机：`enterLong()` 和 `placeGridOrders()` 各调用一次，同一轮下单内缓存结果
- `enterLong` (Level 1 / 首仓)：`baseNotional = balance × base_ratio` (6%)，数量 `baseQty = baseNotional / limitPrice`
- `placeGridOrders` (Level 2-10 / Grid 1-9)：每层 `orderNotional = balance × allocationRatio[i]`，数量 `qty = orderNotional / gridPrice`

## Strategy State Machine

```
States: IDLE → WAITING_ENTRY → IN_POSITION (→ PLACING_GRID) → CLOSING → IDLE
```

| State | Description |
|-------|-------------|
| `IDLE` | 空闲，等待入场信号 |
| `WAITING_ENTRY` | 首仓限价单已挂，等待成交 |
| `IN_POSITION` | 持仓中（可能正在放置网格或已有网格） |
| `PLACING_GRID` | 正在逐层放置网格安全单 |
| `CLOSING` | 正在平仓 |

- `handleTick` 驱动 `IDLE → WAITING_ENTRY` 转换
- 首仓限价单 `EntryTimeout` (10s) 内未成交则撤单回退市价
- TP 成交后 `TPCooldown` (30s) 内不响应新 tick，防止反复开平仓
- `monitorPositionStatus` 检测手动平仓，自动重置状态到 `IDLE`

## Event Types

| Event | Source | Payload |
|-------|--------|---------|
| `EventTick` | `StartPricePolling` | `float64` (price) |
| `EventOrderUpdate` | User stream WS | `*futures.WsOrderTradeUpdate` |
| `EventPositionUpdate` | User stream WS | `*futures.AccountPosition` |
| `EventLog` | Any | `string` |
| `EventStart` | main.go | — |
| `EventStop` | Shutdown | — |

## Web Dashboard

The `internal/api` package serves a browser dashboard with:

- `/` — Dashboard HTML (embedded via `//go:embed`)
- `/api/state` — JSON snapshot of strategy state
- `/api/stream` — SSE stream (pushes state every 2s)
- `/api/login` — Session-based auth (24h HMAC token)
- `/api/pause` / `/api/resume` / `/api/close-all` — Control endpoints
- `/api/klines?interval=1m&limit=200` — OHLCV chart data
- `/api/health` — Health check + public IP

Auth is optional (if `auth_token` is empty, dashboard is open).

## Adding Features

### New Event Type
1. Add constant in `internal/core/event_bus.go`
2. Publish from source component
3. Subscribe in `strategy/strategy.go` handler

### New Strategy State
1. Define in `internal/strategy/strategy.go` as `const StateName State = "NAME"`
2. Add transition logic in appropriate handler
3. Update state machine comments

### New REST API Endpoint
1. Add handler method in `internal/api/server.go`
2. Register route in `Start()` mux
3. Use `s.withAuth(next)` if the endpoint needs authentication

### New Exchange API Method
1. Add method to `BinanceClient` in `internal/exchange/binance.go`
2. Use `bc.client.NewXxxService().Do(context.Background())` pattern
3. Wrap errors with context

## Testing
- No tests exist yet; create `_test.go` files alongside source
- Use table-driven tests
- Mock external dependencies (exchange client, event bus)
