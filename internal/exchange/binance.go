package exchange

import (
	"context"
	"fmt"
	"strconv"
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
}

func NewBinanceClient(cfg *config.ExchangeConfig, bus *core.EventBus) *BinanceClient {
	futures.UseTestnet = cfg.UseTestnet
	client := binance.NewFuturesClient(cfg.ApiKey, cfg.ApiSecret)
	// Enable time synchronization to avoid "Timestamp for this request was 1000ms ahead" errors
	// For futures client, the method might be different or we need to access the embedded BaseClient
	// Actually, go-binance futures client doesn't expose SetTimeOffset directly on the wrapper sometimes.
	// But let's check: NewFuturesClient returns *futures.Client.
	// It seems SetTimeOffset is not exported on futures.Client.
	// We might need to call NewService to sync time manually or just ignore if library handles it.
	// Wait, the library usually does this via:
	// client.NewSetServerTimeService().Do(context.Background())
	// Let's try that instead of the method which might be spot-only.
	
	// Sync time manually
	// client.NewSetServerTimeService().Do(context.Background())
	
	return &BinanceClient{
		client: client,
		cfg:    cfg,
		bus:    bus,
	}
}

// StartUserStream connects to the user data stream (order updates)
func (bc *BinanceClient) StartUserStream() error {
	// Sync Server Time first
	if _, err := bc.client.NewSetServerTimeService().Do(context.Background()); err != nil {
		utils.Logger.Error("Failed to sync server time", zap.Error(err))
	}

	// Connect to User Data Stream (Order Updates)
	listenKey, err := bc.client.NewStartUserStreamService().Do(context.Background())
	if err != nil {
		return fmt.Errorf("failed to start user stream: %w", err)
	}
	utils.Logger.Info("User stream started", zap.String("listenKey", listenKey))

	// User Data WS handler
	wsUserHandler := func(event *futures.WsUserDataEvent) {
		utils.Logger.Info("User stream event received",
			zap.String("event", string(event.Event)),
			zap.String("symbol", bc.cfg.Symbol))
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

	doneC, _, err := futures.WsUserDataServe(listenKey, wsUserHandler, errHandler)
	if err != nil {
		return fmt.Errorf("failed to start user stream: %w", err)
	}
	utils.Logger.Info("User data stream connected")

	// Keep alive user stream every 30m
	go func() {
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if err := bc.client.NewKeepaliveUserStreamService().ListenKey(listenKey).Do(context.Background()); err != nil {
				utils.Logger.Error("Failed to keepalive user stream", zap.Error(err))
			}
		}
	}()

	// Log WS status periodically
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			utils.Logger.Info("WebSocket connections active", zap.String("symbol", bc.cfg.Symbol))
		}
	}()

	_ = doneC

	return nil
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
