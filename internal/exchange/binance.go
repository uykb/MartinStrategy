package exchange

import (
	"context"
	"fmt"
	"strconv"
	"strings"
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

// StartWS connects to the websocket stream
func (bc *BinanceClient) StartWS() error {
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
		switch event.Event {
		case futures.UserDataEventTypeOrderTradeUpdate:
			o := event.OrderTradeUpdate
			utils.Logger.Info("Order Update", zap.String("symbol", o.Symbol), zap.String("status", string(o.Status)))
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

	doneC, stopC, err := futures.WsUserDataServe(listenKey, wsUserHandler, errHandler)
	if err != nil {
		return fmt.Errorf("failed to start user stream: %w", err)
	}
	utils.Logger.Info("User data stream connected")

	// Connect to Market Stream (AggTrade for price)
	// WebSocket stream symbol must be lowercase
	symbolLower := strings.ToLower(bc.cfg.Symbol)
	utils.Logger.Info("Connecting to market stream", zap.String("symbol", symbolLower))

	wsMarketHandler := func(event *futures.WsAggTradeEvent) {
		price, _ := strconv.ParseFloat(event.Price, 64)
		utils.Logger.Info("Tick received from WS", zap.Float64("price", price))
		bc.bus.Publish(core.EventTick, price)
	}

	doneM, stopM, err := futures.WsAggTradeServe(symbolLower, wsMarketHandler, errHandler)
	if err != nil {
		return fmt.Errorf("failed to start market stream: %w", err)
	}
	utils.Logger.Info("Market stream connected", zap.String("symbol", bc.cfg.Symbol))

	go func() {
		<-doneC
		utils.Logger.Warn("User data stream disconnected")
	}()

	go func() {
		<-doneM
		utils.Logger.Warn("Market stream disconnected")
	}()

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

	_ = stopC
	_ = stopM

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
