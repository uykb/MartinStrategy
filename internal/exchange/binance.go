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

// StartWS connects to the websocket stream
func (bc *BinanceClient) StartWS() error {
	// Sync Server Time first
	if _, err := bc.client.NewSetServerTimeService().Do(context.Background()); err != nil {
		utils.Logger.Error("Failed to sync server time", zap.Error(err))
		// We continue anyway, hoping it's a transient network issue
	}

	// Connect to User Data Stream (Order Updates)
	listenKey, err := bc.client.NewStartUserStreamService().Do(context.Background())
	if err != nil {
		return fmt.Errorf("failed to start user stream: %w", err)
	}

	go func() {
		// Keep alive user stream every 30m
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			bc.client.NewKeepaliveUserStreamService().ListenKey(listenKey).Do(context.Background())
		}
	}()

	// User Data WS
	wsUserHandler := func(event *futures.WsUserDataEvent) {
		switch event.Event {
		case futures.UserDataEventTypeOrderTradeUpdate:
			o := event.OrderTradeUpdate
			utils.Logger.Info("Order Update", zap.String("symbol", o.Symbol), zap.String("status", string(o.Status)))
			bc.bus.Publish(core.EventOrderUpdate, o)
		case futures.UserDataEventTypeAccountUpdate:
			// Handle position updates
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
		return err
	}
	
	// Connect to Market Stream (AggTrade for price)
	wsMarketHandler := func(event *futures.WsAggTradeEvent) {
		price, _ := strconv.ParseFloat(event.Price, 64)
		bc.bus.Publish(core.EventTick, price)
	}
	
	doneM, stopM, err := futures.WsAggTradeServe(bc.cfg.Symbol, wsMarketHandler, errHandler)
	if err != nil {
		return err
	}

	go func() {
		<-doneC
		<-doneM
		close(stopC)
		close(stopM)
	}()

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

func (bc *BinanceClient) PlaceOrder(side futures.SideType, orderType futures.OrderType, quantity, price float64) (*futures.CreateOrderResponse, error) {
	service := bc.client.NewCreateOrderService().
		Symbol(bc.cfg.Symbol).
		Side(side).
		Type(orderType).
		Quantity(fmt.Sprintf("%f", quantity))

	if orderType == futures.OrderTypeLimit {
		service.Price(fmt.Sprintf("%f", price)).TimeInForce(futures.TimeInForceTypeGTC)
	}

	return service.Do(context.Background())
}

func (bc *BinanceClient) CancelAllOrders() error {
	return bc.client.NewCancelAllOpenOrdersService().
		Symbol(bc.cfg.Symbol).
		Do(context.Background())
}

func (bc *BinanceClient) GetKlines(limit int) ([]*futures.Kline, error) {
	return bc.client.NewKlinesService().
		Symbol(bc.cfg.Symbol).
		Interval("15m").
		Limit(limit).
		Do(context.Background())
}
