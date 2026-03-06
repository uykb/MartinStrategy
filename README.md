# MartinStrategy (Go Refactor)

纯 Go 语言重构的高性能马丁策略交易机器人，采用 **事件驱动 + 有限状态机 (ED-FSM)** 架构。

## 核心架构

*   **语言**: Go (Golang)
*   **架构**: Event-Driven Finite State Machine (ED-FSM)
*   **交易所**: Binance Futures (go-binance)
*   **存储**: SQLite (持久化) + Redis (分布式锁)
*   **配置**: Viper (支持 YAML/Env)
*   **日志**: Zap (结构化日志)
*   **指标**: go-talib (ATR 计算)

## 目录结构

```
.
├── cmd/
│   └── bot/          # 入口文件
├── internal/
│   ├── config/       # 配置管理 (Viper)
│   ├── core/         # 核心组件 (EventBus, FSM)
│   ├── exchange/     # 交易所适配 (Binance)
│   ├── strategy/     # 策略逻辑 (Martingale)
│   ├── storage/      # 数据存储 (SQLite, Redis)
│   └── utils/        # 工具库 (Logger, Indicators)
├── config.yaml       # 配置文件
├── go.mod            # 依赖管理
└── Dockerfile        # 构建文件
```

## 快速开始

### 1. 配置
修改 `config.yaml` 或使用环境变量：

```yaml
exchange:
  api_key: "YOUR_KEY"
  api_secret: "YOUR_SECRET"
  symbol: "HYPEUSDT"

strategy:
  base_order_size: 100.0
  safety_order_size: 100.0
  max_safety_orders: 5
  target_profit: 0.01
```

### 2. 运行 (Docker)

```bash
docker build -t martin-bot .
docker run -d --name bot martin-bot
```

### 3. 本地运行

```bash
go mod tidy
go run cmd/bot/main.go
```

## 策略逻辑 (FSM)

系统基于状态机运行，主要状态包括：
1.  **IDLE**: 空仓等待。
2.  **IN_POSITION**: 持仓中，监控网格和止盈。
3.  **PLACING_GRID**: 正在批量挂单。

事件流转：
*   `Tick` -> 触发 `EnterLong` (IDLE) 或 `CheckPnL` (IN_POSITION)。
*   `OrderUpdate` -> 触发 `PlaceGrid` (Base Filled) 或 `UpdateTP` (Safety Filled) 或 `Reset` (TP Filled)。

## 注意事项

*   需要 Redis 服务运行 (默认 `localhost:6379`)。
*   默认配置为 `HYPEUSDT`。
*   请确保 API Key 有合约交易权限。
