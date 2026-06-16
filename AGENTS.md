# AGENTS.md

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

# Docker
docker-compose build
docker-compose up -d
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
- Struct tags use `mapstructure` for config, `json`/`gorm` for storage models

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
| `internal/strategy` | Martingale FSM (states: IDLE → PLACING_GRID → IN_POSITION) |
| `internal/storage` | GORM + SQLite, Redis for locking |
| `internal/utils` | Indicators (ATR), rounding, Zap logger |

## Key Constants
- `MinOrderValue = 6.0` - Minimum USDT order value for Binance Futures (动态头仓下限)
- Event queue buffer: 1000
- Grid levels: 8 max (30m/1h/2h/4h/8h/12h/1d/1w, Fibonacci scaled)
- Price polling interval: 10s
- Position monitor interval: 5s
- Grid order API rate limit: 200ms between orders
- User stream keepalive: 15m (Binance requires within 60m)
- Server time sync: every 5m
- WebSocket reconnect: up to 5 retries with exponential backoff

## Key Config Parameters
- `base_ratio: 0.08` - 头仓金额 = 账户 USDT 余额 × base_ratio（动态计算，每次开仓前实时查询）
- `max_safety_orders: 8` - 最大网格层数
- `atr_period: 14` - ATR 计算周期

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
- `enterLong` 中计算 `unitQty = calcMinNotional() / currentPrice`
- `placeGridOrders` 中计算 `unitQty = calcMinNotional() / entryPrice`，循环内复用该值

## Adding Features

### New Event Type
1. Add constant in `internal/core/event_bus.go`
2. Publish from source component
3. Subscribe in `strategy/strategy.go` handler

### New Strategy State
1. Define in `internal/strategy/strategy.go` as `const StateName State = "NAME"`
2. Add transition logic in appropriate handler
3. Update state machine comments

### New REST API Method
1. Add method to `BinanceClient` in `internal/exchange/binance.go`
2. Use `bc.client.NewXxxService().Do(context.Background())` pattern
3. Wrap errors with context

## Testing
- No tests exist yet; create `_test.go` files alongside source
- Use table-driven tests
- Mock external dependencies (exchange client, storage)
