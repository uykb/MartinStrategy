package main

import (
	"io"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/uykb/MartinStrategy/internal/config"
	"github.com/uykb/MartinStrategy/internal/core"
	"github.com/uykb/MartinStrategy/internal/exchange"
	"github.com/uykb/MartinStrategy/internal/strategy"
	"github.com/uykb/MartinStrategy/internal/utils"
	"go.uber.org/zap"

	"github.com/uykb/MartinStrategy/internal/api"
)

// fetchPublicIP queries http://ipinfo.io/ip to get the server's public IP address.
func fetchPublicIP() string {
	resp, err := http.Get("http://ipinfo.io/ip")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(body))
}

func main() {
	// 1. Config (YAML config + MARTIN_ env overrides)
	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		panic(err)
	}

	// 2. Logger
	if err := utils.InitLogger(cfg.Log.Level); err != nil {
		panic(err)
	}
	defer utils.Logger.Sync()
	utils.Logger.Info("Starting MartinStrategy Bot", zap.String("symbol", cfg.Exchange.Symbol))

	// 2.5 Log public IP for Binance API whitelist
	if ip := fetchPublicIP(); ip != "" {
		utils.Logger.Info("容器公网 IP（请加入 Binance API 白名单）", zap.String("ip", ip))
	} else {
		utils.Logger.Warn("无法获取公网 IP，请手动查询并加入 Binance API 白名单")
	}

	// 3. Event Bus
	bus := core.NewEventBus()
	bus.Start()
	defer bus.Stop()

	// 4. Exchange
	ex := exchange.NewBinanceClient(&cfg.Exchange, bus)
	if err := ex.StartUserStream(); err != nil {
		utils.Logger.Fatal("Failed to start user stream", zap.Error(err))
	}

	// 5. Strategy (同步初始化并完成仓位与挂单状态同步)
	strat := strategy.NewMartingaleStrategy(&cfg.Strategy, ex, bus)
	strat.Start()

	// 6. 状态同步完成后，再启动价格轮询（避免启动初期误判 IDLE 开新仓）
	go ex.StartPricePolling(10 * time.Second)

	// 7. Web Dashboard (API server with SSE)
	if cfg.API.Enabled {
		apiSrv := api.NewServer(strat, cfg.API.Port, cfg.API.AuthToken)
		go func() {
			if err := apiSrv.Start(); err != nil {
				utils.Logger.Error("API server error", zap.Error(err))
			}
		}()
		utils.Logger.Info("Dashboard started", zap.Int("port", cfg.API.Port))
	}

	// 8. Wait for signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	utils.Logger.Info("Shutting down...")

	ex.StopUserStream()

	// Graceful shutdown: 只有在明确无持仓（amt == 0 且查询成功）时才清理残留挂单
	// 只要检测到持仓，或者查询持仓出错时，坚决保留所有持仓与挂单，绝不平仓，绝不撤单！
	pos, posErr := ex.GetPosition()
	if posErr != nil {
		utils.Logger.Warn("Shutdown: failed to query position, safely preserving all orders and positions", zap.Error(posErr))
	} else {
		amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
		if math.Abs(amt) == 0 {
			utils.Logger.Info("Shutdown: no open position detected, cancelling lingering orders")
			if err := ex.CancelAllOrders(); err != nil {
				utils.Logger.Error("Failed to cancel orders on shutdown", zap.Error(err))
			} else {
				utils.Logger.Info("All lingering orders cancelled on clean shutdown")
			}
		} else {
			utils.Logger.Info("Shutdown: open position detected, safely preserving all positions and orders across restarts",
				zap.Float64("position_amt", amt))
		}
	}

	utils.Logger.Info("Shutdown complete")
}
