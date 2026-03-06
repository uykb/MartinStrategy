package main

import (
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
	// 1. Config (Load from Env Vars only)
	// We pass an empty string to indicate no config file, forcing Viper to use defaults or env vars
	// However, our LoadConfig currently expects a file. Let's modify it to be optional or just rely on env.
	// For now, we assume config.yaml might still exist in Docker container (COPY config.yaml .), 
	// so we hardcode "config.yaml" as the default location in Docker.
	cfg, err := config.LoadConfig("config.yaml")
	if err != nil {
		// In Docker, if config.yaml is missing, we might want to proceed if ENV vars are set.
		// But LoadConfig implementation needs to handle "file not found" gracefully.
		// Let's assume for now config.yaml is COPY-ed in Dockerfile.
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
	if err := ex.CancelAllOrders(); err != nil {
		utils.Logger.Error("Failed to cancel orders on shutdown", zap.Error(err))
	} else {
		utils.Logger.Info("All orders cancelled")
	}
	
	// Close DB connections if needed
	// db.Redis.Close()
	
	utils.Logger.Info("Shutdown complete")
}
