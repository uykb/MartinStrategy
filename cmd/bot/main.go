package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/uykb/MartinStrategy/internal/config"
	"github.com/uykb/MartinStrategy/internal/core"
	"github.com/uykb/MartinStrategy/internal/exchange"
	"github.com/uykb/MartinStrategy/internal/storage"
	"github.com/uykb/MartinStrategy/internal/strategy"
	"github.com/uykb/MartinStrategy/internal/utils"
	"go.uber.org/zap"
)

func main() {
	configFile := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	// 1. Config
	cfg, err := config.LoadConfig(*configFile)
	if err != nil {
		// If config file missing, we rely on env vars, so just log warning
		// But LoadConfig returns error if file not found when explicitly set.
		// For simplicity, we assume we might need env vars only.
		// But let's print error for now.
		panic(err)
	}

	// 2. Logger
	if err := utils.InitLogger(cfg.Log.Level); err != nil {
		panic(err)
	}
	defer utils.Logger.Sync()
	utils.Logger.Info("Starting MartinStrategy Bot", zap.String("symbol", cfg.Exchange.Symbol))

	// 3. Storage
	db, err := storage.InitStorage(cfg.Storage.SqlitePath, cfg.Storage.RedisAddr, cfg.Storage.RedisPass, cfg.Storage.RedisDB)
	if err != nil {
		utils.Logger.Fatal("Failed to init storage", zap.Error(err))
	}

	// 4. Event Bus
	bus := core.NewEventBus()
	bus.Start()
	defer bus.Stop()

	// 5. Exchange
	ex := exchange.NewBinanceClient(&cfg.Exchange, bus)
	if err := ex.StartWS(); err != nil {
		utils.Logger.Fatal("Failed to start exchange WS", zap.Error(err))
	}

	// 6. Strategy
	strat := strategy.NewMartingaleStrategy(&cfg.Strategy, ex, db, bus)
	go strat.Start()

	// 7. Wait for signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	utils.Logger.Info("Shutting down...")
	
	// Graceful shutdown logic (e.g. CancelAllOrders if needed)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := ex.CancelAllOrders(); err != nil {
		utils.Logger.Error("Failed to cancel orders on shutdown", zap.Error(err))
	} else {
		utils.Logger.Info("All orders cancelled")
	}
	
	// Close DB connections if needed
	// db.Redis.Close()
	
	utils.Logger.Info("Shutdown complete")
}
