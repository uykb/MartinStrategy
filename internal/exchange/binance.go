package exchange

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/futures"
	"github.com/uykb/MartinStrategy/internal/config"
	"github.com/uykb/MartinStrategy/internal/core"
	"github.com/uykb/MartinStrategy/internal/utils"
	"go.uber.org/zap"
)

type BinanceClient struct {
	client *futures.Client
	cfg    *config.ExchangeConfig
	bus    *core.EventBus

	// User stream management
	userStreamMu     sync.Mutex
	userStreamStopCh chan struct{}
	userStreamDoneCh chan struct{}
	listenKey        string
}

func NewBinanceClient(cfg *config.ExchangeConfig, bus *core.EventBus) *BinanceClient {
	futures.UseTestnet = cfg.UseTestnet
	client := binance.NewFuturesClient(cfg.ApiKey, cfg.ApiSecret)

	return &BinanceClient{
		client:         client,
		cfg:            cfg,
		bus:            bus,
		userStreamStopCh: make(chan struct{}),
		userStreamDoneCh: make(chan struct{}),
	}
}

// StartUserStream connects to the user data stream (order updates) with auto-reconnect
func (bc *BinanceClient) StartUserStream() error {
	bc.userStreamMu.Lock()
	defer bc.userStreamMu.Unlock()

	if bc.userStreamStopCh != nil {
		close(bc.userStreamStopCh)
	}
	bc.userStreamStopCh = make(chan struct{})
	bc.userStreamDoneCh = make(chan struct{})

	// Sync server time at startup
	if offset, err := bc.client.NewSetServerTimeService().Do(context.Background()); err != nil {
		utils.Logger.Error("Failed to sync server time", zap.Error(err))
	} else {
		utils.Logger.Info("Server time synced", zap.Int64("offset_ms", offset))
	}

	// Start periodic time sync every 5 minutes
	go bc.periodicTimeSync()

	// Start user stream with retry logic
	return bc.connectUserStreamWithRetry()
}

func (bc *BinanceClient) connectUserStreamWithRetry() error {
	maxRetries := 5
	retryDelay := 2 * time.Second

	for attempt := 1; attempt <= maxRetries; attempt++ {
		listenKey, err := bc.client.NewStartUserStreamService().Do(context.Background())
		if err != nil {
			utils.Logger.Error("Failed to start user stream",
				zap.Int("attempt", attempt),
				zap.Error(err))
			if attempt < maxRetries {
				time.Sleep(retryDelay * time.Duration(attempt))
				continue
			}
			return fmt.Errorf("failed to start user stream after %d attempts: %w", maxRetries, err)
		}

		bc.listenKey = listenKey
		utils.Logger.Info("User stream started", zap.String("listenKey", listenKey))
		break
	}

	wsUserHandler := func(event *futures.WsUserDataEvent) {
		switch event.Event {
		case futures.UserDataEventTypeOrderTradeUpdate:
			o := event.OrderTradeUpdate
			utils.Logger.Info("Order Trade Update received",
				zap.String("symbol", o.Symbol),
				zap.String("status", string(o.Status)),
				zap.String("side", string(o.Side)),
				zap.Int64("orderId", o.ID))
			bc.bus.Publish(core.EventOrderUpdate, &o)
		case futures.UserDataEventTypeAccountUpdate:
			for _, p := range event.AccountUpdate.Positions {
				if p.Symbol == bc.cfg.Symbol {
					bc.bus.Publish(core.EventPositionUpdate, p)
				}
			}
		}
	}

	errHandler := func(err error) {
		utils.Logger.Error("WS Error", zap.Error(err))
	}

	doneC, _, err := futures.WsUserDataServe(bc.listenKey, wsUserHandler, errHandler)
	if err != nil {
		return fmt.Errorf("failed to start user stream: %w", err)
	}
	utils.Logger.Info("User data stream connected")

	// Keep alive user stream every 15m (Binance requires keepalive within 60m)
	go bc.keepUserStreamAlive()

	// Monitor connection and reconnect if needed
	go bc.monitorUserStream(doneC)

	return nil
}

func (bc *BinanceClient) periodicTimeSync() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-bc.userStreamStopCh:
			return
		case <-ticker.C:
			if offset, err := bc.client.NewSetServerTimeService().Do(context.Background()); err != nil {
				utils.Logger.Error("Periodic time sync failed", zap.Error(err))
			} else {
				utils.Logger.Debug("Periodic time sync", zap.Int64("offset_ms", offset))
			}
		}
	}
}

func (bc *BinanceClient) keepUserStreamAlive() {
	// Binance requires keepalive within 60 minutes, we use 15 minutes for safety
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-bc.userStreamStopCh:
			utils.Logger.Info("keepUserStreamAlive: stopping")
			return
		case <-ticker.C:
			bc.userStreamMu.Lock()
			listenKey := bc.listenKey
			bc.userStreamMu.Unlock()

			if listenKey == "" {
				continue
			}

			if err := bc.client.NewKeepaliveUserStreamService().ListenKey(listenKey).Do(context.Background()); err != nil {
				utils.Logger.Error("Failed to keepalive user stream", zap.Error(err))
			} else {
				utils.Logger.Debug("User stream keepalive successful")
			}
		}
	}
}

func (bc *BinanceClient) monitorUserStream(doneC <-chan struct{}) {
	defer close(bc.userStreamDoneCh)

	select {
	case <-doneC:
		utils.Logger.Warn("User data stream disconnected, attempting reconnect...")
	case <-bc.userStreamStopCh:
		utils.Logger.Info("monitorUserStream: stopping")
		return
	}

	// Wait before reconnecting with exponential backoff
	backoff := 2 * time.Second
	for i := 0; i < 5; i++ {
		bc.userStreamMu.Lock()
		stopCh := bc.userStreamStopCh
		bc.userStreamMu.Unlock()

		select {
		case <-stopCh:
			return
		default:
			utils.Logger.Info("Reconnecting to user stream...",
				zap.Int("attempt", i+1),
				zap.Duration("backoff", backoff))
			time.Sleep(backoff)

			if err := bc.connectUserStreamWithRetry(); err != nil {
				utils.Logger.Error("Failed to reconnect user stream", zap.Error(err))
				backoff *= 2 // Exponential backoff
				continue
			}
			utils.Logger.Info("Successfully reconnected to user stream")
			return
		}
	}

	utils.Logger.Error("Failed to reconnect user stream after multiple attempts")
}

func (bc *BinanceClient) StopUserStream() {
	bc.userStreamMu.Lock()
	defer bc.userStreamMu.Unlock()

	if bc.userStreamStopCh != nil {
		close(bc.userStreamStopCh)
		bc.userStreamStopCh = nil
	}
	utils.Logger.Info("User stream stopped")
}

func (bc *BinanceClient) GetPosition() (*futures.AccountPosition, error) {
	acc, err := bc.client.NewGetAccountService().Do(context.Background())
	if err != nil {
		return nil, err
	}
	for _, p := range acc.Positions {
		if p.Symbol == bc.cfg.Symbol {
			return p, nil
		}
	}
	return nil, fmt.Errorf("position not found for %s", bc.cfg.Symbol)
}

func (bc *BinanceClient) GetSymbol() string {
	return bc.cfg.Symbol
}

func (bc *BinanceClient) GetExchangeInfo() (*futures.ExchangeInfo, error) {
	return bc.client.NewExchangeInfoService().Do(context.Background())
}

func (bc *BinanceClient) PlaceOrder(side futures.SideType, orderType futures.OrderType, quantity, price float64) (*futures.CreateOrderResponse, error) {
	qtyStr := strconv.FormatFloat(quantity, 'f', -1, 64)
	service := bc.client.NewCreateOrderService().
		Symbol(bc.cfg.Symbol).
		Side(side).
		Type(orderType).
		Quantity(qtyStr)

	if orderType == futures.OrderTypeLimit {
		priceStr := strconv.FormatFloat(price, 'f', -1, 64)
		service.Price(priceStr).TimeInForce(futures.TimeInForceTypeGTC)
	}

	return service.Do(context.Background())
}

func (bc *BinanceClient) CancelAllOrders() error {
	return bc.client.NewCancelAllOpenOrdersService().
		Symbol(bc.cfg.Symbol).
		Do(context.Background())
}

func (bc *BinanceClient) CancelOrder(orderID int64) error {
	_, err := bc.client.NewCancelOrderService().
		Symbol(bc.cfg.Symbol).
		OrderID(orderID).
		Do(context.Background())
	return err
}

func (bc *BinanceClient) GetOpenOrders() ([]*futures.Order, error) {
	return bc.client.NewListOpenOrdersService().
		Symbol(bc.cfg.Symbol).
		Do(context.Background())
}

func (bc *BinanceClient) GetKlines(interval string, limit int) ([]*futures.Kline, error) {
	return bc.client.NewKlinesService().
		Symbol(bc.cfg.Symbol).
		Interval(interval).
		Limit(limit).
		Do(context.Background())
}

func (bc *BinanceClient) GetLatestPrice() (float64, error) {
	klines, err := bc.client.NewKlinesService().
		Symbol(bc.cfg.Symbol).
		Interval("1m").
		Limit(1).
		Do(context.Background())
	if err != nil {
		return 0, err
	}
	if len(klines) == 0 {
		return 0, fmt.Errorf("no kline data")
	}
	price, err := strconv.ParseFloat(klines[0].Close, 64)
	if err != nil {
		return 0, err
	}
	return price, nil
}

func (bc *BinanceClient) StartPricePolling(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	utils.Logger.Info("Price polling started", zap.Duration("interval", interval))

	for range ticker.C {
		price, err := bc.GetLatestPrice()
		if err != nil {
			utils.Logger.Error("Failed to get latest price", zap.Error(err))
			continue
		}
		bc.bus.Publish(core.EventTick, price)
	}
}
