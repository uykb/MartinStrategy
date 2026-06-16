# MartinStrategy

基于 Go 语言的高性能马丁格尔策略交易机器人，采用 **事件驱动 + 有限状态机 (ED-FSM)** 架构，专为 Binance Futures（合约）设计。

## 特性

- **事件驱动架构**: 基于 EventBus 的异步消息处理，解耦数据源与策略逻辑
- **有限状态机**: 清晰的状态流转（IDLE → PLACING_GRID → IN_POSITION），避免逻辑混乱
- **动态头仓**: 头仓金额根据账户总资产与配置比例（base_ratio）动态计算，随盈利自动放大
- **八级 ATR 网格**: 网格间距采用 30m/1h/2h/4h/8h/12h/1d/1w 八级时间框架 ATR 动态计算
- **Fibonacci 加仓**: 安全订单数量按 Fibonacci 序列（1,1,2,3,5,8,13,21）递增
- **并发安全**: TryLock 防重入锁 + 状态标志双重保护，防止重复下单
- **状态恢复**: 启动时自动同步交易所持仓与挂单状态，支持意外重启恢复
- **自动重连**: WebSocket 断线指数退避重连，定时时间同步与心跳保活
- **生产就绪**: 结构化日志、错误处理、监控计数器、优雅关闭

## 目录结构

```
.
├── cmd/
│   └── bot/
│       └── main.go              # 程序入口，初始化与生命周期管理
├── internal/
│   ├── config/
│   │   └── config.go            # 配置管理 (Viper: YAML + 环境变量)
│   ├── core/
│   │   └── event_bus.go         # 事件总线 (Pub/Sub 模式)
│   ├── exchange/
│   │   └── binance.go           # Binance Futures 客户端 (REST + WebSocket)
│   ├── strategy/
│   │   └── strategy.go          # 马丁格尔策略核心 (FSM 状态机)
│   ├── storage/
│   │   └── storage.go           # 数据存储 (SQLite + Redis 分布式锁)
│   └── utils/
│       ├── indicators.go        # 技术指标 (ATR) 与数量/价格精度处理
│       └── logger.go            # Zap 结构化日志
├── config.yaml                  # 默认配置文件
├── Dockerfile                   # 多阶段 Docker 构建
├── docker-compose.yml           # Docker Compose 编排
├── .github/workflows/
│   └── docker-publish.yml       # CI/CD 自动构建与推送
├── go.mod                       # Go 模块依赖
├── README.md                    # 项目文档
└── AGENTS.md                    # 开发者/Agent 指南
```

## 架构概览

```
┌─────────────────────┐      Events       ┌──────────────────┐
│   BinanceClient     │ ─────────────────▶│     EventBus     │
│   (交易所层)         │  (TICK, ORDER)    │    (核心层)       │
│                     │                   │                  │
│ • REST API          │                   │ • Pub/Sub        │
│ • WebSocket 用户流   │                   │ • 异步处理        │
│ • 自动重连           │                   │ • 缓冲队列(1000)  │
│ • 价格轮询           │                   └────────┬─────────┘
└─────────────────────┘                            │
                                          Subscribe │
                                                    ▼
                                           ┌──────────────────┐
                                           │ MartingaleStrategy│
                                           │   (策略层/FSM)    │
                                           │                  │
                                           │ • 状态机          │
                                           │ • 网格下单        │
                                           │ • 止盈管理        │
                                           │ • 仓位监控        │
                                           └────────┬─────────┘
                                                    │
                                           ┌────────┴─────────┐
                                           │   Storage Layer   │
                                           │   (存储层)         │
                                           │                  │
                                           │ • SQLite (GORM)  │
                                           │ • Redis (分布式锁)│
                                           └──────────────────┘
```

## 快速开始

### 方式一: Docker Compose（推荐）

```bash
# 1. 创建配置文件
cp config.yaml.example config.yaml   # 如无 example 文件，直接编辑 config.yaml

# 2. 编辑配置（填入 API 密钥）
vim config.yaml

# 3. 启动服务
docker-compose up -d

# 4. 查看日志
docker-compose logs -f
```

### 方式二: 本地运行

```bash
# 1. 安装依赖
go mod tidy

# 2. 编辑配置
vim config.yaml

# 3. 运行
go run cmd/bot/main.go
```

### 方式三: 构建二进制

```bash
go build -o bot cmd/bot/main.go
./bot
```

## 配置说明

### config.yaml

```yaml
exchange:
  api_key: ""              # Binance API Key
  api_secret: ""           # Binance API Secret
  symbol: "HYPEUSDT"       # 交易对
  use_testnet: false       # 是否使用测试网

strategy:
  max_safety_orders: 8     # 最大加仓层数 (Fibonacci)
  atr_period: 14           # ATR 周期
  base_ratio: 0.08         # 头仓占总资产比例（动态计算）

storage:
  sqlite_path: "bot.db"    # SQLite 数据库路径
  redis_addr: "localhost:6379"
  redis_pass: ""
  redis_db: 0

log:
  level: "info"            # 日志级别: debug, info, warn, error
```

### 环境变量

支持通过环境变量覆盖配置，前缀为 `MARTIN_`：

```bash
export MARTIN_EXCHANGE_API_KEY="your_api_key"
export MARTIN_EXCHANGE_API_SECRET="your_api_secret"
export MARTIN_EXCHANGE_SYMBOL="BTCUSDT"
export MARTIN_EXCHANGE_USE_TESTNET="true"
export MARTIN_STRATEGY_MAX_SAFETY_ORDERS="8"
export MARTIN_STRATEGY_BASE_RATIO="0.08"
export MARTIN_LOG_LEVEL="debug"
```

## 策略详解

### 状态机流转

```
┌──────────┐     Tick 事件      ┌───────────────┐
│  IDLE    │ ──────────────────▶│ PLACING_GRID  │
│ (空仓等待) │                    │  (等待底仓成交)  │
└──────────┘                    └───────┬───────┘
     ▲                                  │
     │                                  │ 检测到持仓
     │                                  ▼
     │                          ┌───────────────┐
     │     TP 成交 (SELL)       │ IN_POSITION   │
     └─────────────────────────│   (持仓中)     │
                               │               │
                               │  安全单成交    │
                               ▼               │
                         更新止盈单 (TP)        │
                               ▲               │
                               └───────────────┘
```

| 状态 | 说明 | 触发进入 | 触发离开 |
|------|------|----------|----------|
| `IDLE` | 空仓等待，可接收新 Tick | 启动/TP 成交/手动平仓 | 收到 Tick 事件 |
| `PLACING_GRID` | 底仓已下，等待成交 | 市价买入底仓后 | 检测到持仓 |
| `IN_POSITION` | 持有仓位，网格单活跃 | 底仓成交后 | 止盈成交/手动平仓 |
| `CLOSING` | 定义但未主动使用 | - | - |

### 交易流程

1. **开仓 (enterLong)**: IDLE 状态收到 Tick 事件，查询账户 USDT 余额，按 `余额 × base_ratio` 动态计算头仓金额（不低于 6 USDT），市价买入
2. **等待成交 (waitForFillAndPlaceGrid)**: 每 2 秒轮询检查持仓，最多等待 30 秒，检测到持仓后触发网格下单
3. **网格下单 (placeGridOrders)**: 根据八级时间框架 ATR 计算间距，Fibonacci 序列计算数量，放置限价安全单
4. **止盈管理 (updateTP)**: 基于 30 分钟 ATR 计算止盈价格，均价 + ATR(30m) 放置限价卖出单
5. **加仓处理**: 安全单成交后，重新计算均价与止盈价格，更新 TP 订单
6. **止盈退出**: TP 订单成交后，取消所有挂单，状态重置为 IDLE，准备下一轮

### 网格间距设计

网格采用**八级时间框架 ATR**设计，每层直接使用对应周期的 ATR 值作为间距：

| 层级 | 间距计算 | 时间框架 | 说明 |
|------|----------|----------|------|
| 1 | ATR(30m) | 30 分钟 | 首层保护 |
| 2 | ATR(1h) | 1 小时 | 第二层保护 |
| 3 | ATR(2h) | 2 小时 | 中短期保护 |
| 4 | ATR(4h) | 4 小时 | 中期保护 |
| 5 | ATR(8h) | 8 小时 | 中长期保护 |
| 6 | ATR(12h) | 12 小时 | 长期保护 |
| 7 | ATR(1d) | 日线 | 长期保护 |
| 8 | ATR(1w) | 周线 | 最深层保护 |

> 间距为**相对上一层**的距离，非绝对距离。ATR 获取失败时回退至入场价 × 1%。

### Fibonacci 加仓数量

| 层级 | Fibonacci 倍数 | 数量（假设 unit=1） | 累计倍数 |
|------|----------------|---------------------|----------|
| 1 | 1 | 1 | 1 |
| 2 | 1 | 1 | 2 |
| 3 | 2 | 2 | 4 |
| 4 | 3 | 3 | 7 |
| 5 | 5 | 5 | 12 |
| 6 | 8 | 8 | 20 |
| 7 | 13 | 13 | 33 |
| 8 | 21 | 21 | 54 |

> 每层数量 = unitQty × Fibonacci(n)，unitQty 由动态头仓金额（余额 × base_ratio）/ 入场价计算并向上取整至 stepSize。

### 止盈策略

- **计算基准**: 当前持仓均价（EntryPrice）
- **止盈价格**: avgPrice + ATR(30m)
- **止盈数量**: 全仓平出
- **更新时机**: 每次安全单成交后重新计算并替换 TP 订单

## 并发安全机制

### 1. TryLock 防重入锁

```go
// placeGridOrders 防并发
if !s.gridMu.TryLock() {
    s.gridSkipCount++  // 监控计数
    return
}
defer s.gridMu.Unlock()

// updateTP 防并发
if !s.tpMu.TryLock() {
    s.tpSkipCount++    // 监控计数
    return
}
defer s.tpMu.Unlock()
```

### 2. 状态标志双重检查

```go
// 第一重：无锁快速检查
s.mu.RLock()
if s.gridPlaced {
    s.mu.RUnlock()
    return
}
s.mu.RUnlock()

// 第二重：获取锁后再次检查
if !s.gridMu.TryLock() { ... }
s.mu.RLock()
if s.gridPlaced { ... }  // 防止等待锁期间被其他 goroutine 设置
s.mu.RUnlock()
```

### 3. 状态原子操作与失败回滚

```go
// 状态检查与变更（锁内）
s.mu.Lock()
if s.currentState != StateIdle {
    s.mu.Unlock()
    return
}
s.currentState = StatePlacingGrid
s.mu.Unlock()

// 网络请求（锁外执行，避免阻塞）
if err := s.enterLong(price); err != nil {
    s.mu.Lock()
    s.currentState = StateIdle  // 失败回滚
    s.mu.Unlock()
}
```

### 4. 执行价格优化

从订单成交事件直接获取执行价格，避免 Position API 的竞态条件：

```go
// 从 WebSocket 事件获取（推荐）
execPrice, _ := strconv.ParseFloat(order.AveragePrice, 64)
go s.placeGridOrders(execPrice)
```

## 核心模块说明

### EventBus (internal/core/event_bus.go)

| 组件 | 说明 |
|------|------|
| 事件类型 | TICK, ORDER_UPDATE, POSITION_UPDATE, LOG, START, STOP |
| 队列容量 | 1000（满时丢弃事件） |
| 处理方式 | 异步 goroutine 并行处理 |
| 线程安全 | sync.RWMutex 保护 handler 映射 |

### BinanceClient (internal/exchange/binance.go)

| 功能 | 说明 |
|------|------|
| WebSocket 用户流 | 实时订单更新与账户更新 |
| 自动重连 | 指数退避（最多 5 次重试） |
| 心跳保活 | 每 15 分钟保活（Binance 要求 60 分钟内） |
| 时间同步 | 启动时 + 每 5 分钟定时同步 |
| 价格轮询 | 每 10 秒 REST API 轮询（发布 TICK 事件） |
| REST API | PlaceOrder, CancelOrder, CancelAllOrders, GetPosition, GetOpenOrders, GetKlines, GetExchangeInfo, GetBalance |

### MartingaleStrategy (internal/strategy/strategy.go)

| 组件 | 说明 |
|------|------|
| 动态头仓 | `余额 × base_ratio`，不低于 MinOrderValue (6.0 USDT) |
| 网格层级 | 最多 8 层（可配置） |
| 仓位监控 | 每 5 秒检查一次（检测手动平仓） |
| 状态同步 | 启动时自动恢复持仓与挂单状态 |
| API 限频保护 | 网格下单间隔 200ms |

### Storage (internal/storage/storage.go)

| 组件 | 说明 |
|------|------|
| SQLite | GORM ORM，自动迁移，存储订单历史与机器人状态 |
| Redis | SetNX 分布式锁（TTL 过期） |

### Utils (internal/utils/)

| 函数 | 说明 |
|------|------|
| CalculateATR | 基于 go-talib 计算平均真实波幅 |
| ToFixed | float64 固定精度舍入 |
| RoundUpToTickSize | 向上取整至最小变动价位（用于数量） |
| RoundToTickSize | 四舍五入至最小变动价位（用于价格） |

## 监控指标

### 日志关键字段

| 字段 | 说明 |
|------|------|
| `balance` | 账户 USDT 余额 |
| `ratio` | 头仓比例 (base_ratio) |
| `notional` | 动态计算的头仓金额 |
| `skip_count` | 因并发冲突跳过的次数 |
| `entryPrice` | 入场价格 |
| `ATR30m` | 30 分钟 ATR 值 |
| `UnitQty` | 单位数量 |
| `state` | 当前状态机状态 |
| `gridPlaced` | 网格是否已放置标志 |

### 示例日志

```json
{"level":"info","msg":"Dynamic MinNotional","balance":1000,"ratio":0.08,"notional":80}
{"level":"info","msg":"Tick received","price":39.638,"state":"IDLE","gridPlaced":false}
{"level":"info","msg":"Order Update Received","id":123456,"status":"FILLED","type":"LIMIT"}
{"level":"info","msg":"Using execution price from order event","entryPrice":39.638}
{"level":"warn","msg":"placeGridOrders skipped: already running","skip_count":5}
{"level":"warn","msg":"updateTP skipped: already running","skip_count":12}
```

## 技术栈

| 组件 | 技术 | 版本 |
|------|------|------|
| 语言 | Go | 1.25+ |
| 交易所 | Binance Futures (go-binance) | v2.8.11 |
| 存储 | SQLite (glebarez/sqlite) | v1.11.0 |
| 缓存/锁 | Redis (go-redis) | v9.18.0 |
| 配置 | Viper | v1.21.0 |
| 日志 | Zap | v1.27.1 |
| ORM | GORM | v1.31.1 |
| 技术指标 | go-talib | - |

## 开发

```bash
# 运行测试
go test ./...

# 运行单个测试
go test -v -run TestCalculateATR ./internal/utils/

# 构建
go build -o bot cmd/bot/main.go

# 代码检查
go vet ./...
go fmt ./...

# 依赖管理
go mod tidy
go mod download

# Docker
docker-compose build
docker-compose up -d
```

## CI/CD

项目使用 GitHub Actions 自动构建并推送镜像到 GitHub Container Registry：

- **触发条件**: push/PR 到 main 分支
- **构建平台**: linux/amd64, linux/arm64
- **镜像地址**: `ghcr.io/<owner>/martinstrategy:latest`

## 风险提示

- 马丁格尔策略在单边下跌行情中风险极高，可能导致重大亏损
- 建议设置止损或限制最大持仓层数
- 请确保 API Key 具有合约交易权限
- 强烈建议先在测试网（use_testnet: true）验证策略
- 本软件仅供学习研究，不构成投资建议，使用风险自负

## License

MIT License