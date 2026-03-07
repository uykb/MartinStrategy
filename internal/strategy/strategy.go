package strategy

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/futures"
	"github.com/uykb/MartinStrategy/internal/config"
	"github.com/uykb/MartinStrategy/internal/core"
	"github.com/uykb/MartinStrategy/internal/exchange"
	"github.com/uykb/MartinStrategy/internal/storage"
	"github.com/uykb/MartinStrategy/internal/utils"
	"go.uber.org/zap"
)

// State definition
type State string

const (
	StateIdle        State = "IDLE"
	StateInPosition  State = "IN_POSITION"
	StatePlacingGrid State = "PLACING_GRID"
	StateClosing     State = "CLOSING"
)

// MinNotional is the minimum order value in USDT for Binance Futures
const MinNotional = 5.0

type MartingaleStrategy struct {
	cfg      *config.StrategyConfig
	exchange *exchange.BinanceClient
	storage  *storage.Database
	bus      *core.EventBus

	mu           sync.RWMutex
	currentState State
	position     *futures.AccountPosition
	activeOrders map[int64]*futures.Order // Local cache of active orders
	currentTPOrderID int64
	
	currentATR   float64
	
	// Symbol Info
	quantityPrecision int
	pricePrecision    int
	minQty            float64
	stepSize          float64 // For quantity
	tickSize          float64 // For price
}

func NewMartingaleStrategy(cfg *config.StrategyConfig, ex *exchange.BinanceClient, st *storage.Database, bus *core.EventBus) *MartingaleStrategy {
	return &MartingaleStrategy{
		cfg:          cfg,
		exchange:     ex,
		storage:      st,
		bus:          bus,
		currentState: StateIdle,
		activeOrders: make(map[int64]*futures.Order),
	}
}

func (s *MartingaleStrategy) Start() {
	// Initialize Symbol Info (Precision, etc.)
	if err := s.initSymbolInfo(); err != nil {
		utils.Logger.Fatal("Failed to init symbol info", zap.Error(err))
	}

	// Subscribe to events
	s.bus.Subscribe(core.EventTick, s.handleTick)
	s.bus.Subscribe(core.EventOrderUpdate, s.handleOrderUpdate)
	
	// Initial state sync
	s.syncState()
}

func (s *MartingaleStrategy) initSymbolInfo() error {
	// We need to fetch Exchange Info
	// For now, let's assume default values or fetch it
	// TODO: Implement GetExchangeInfo in BinanceClient
	// Hardcoded for HYPEUSDT/USDT pairs usually:
	// Price Precision: 2 (0.01) or 4 (0.0001)
	// Qty Precision: 2 (0.01) or 3 (0.001) or 0 (1)
	
	// Assuming HYPEUSDT has:
	// Price Precision: 3 (0.001) based on user input
	// Qty Precision: 0 (1.0) or 1 (0.1) - let's assume 0.1 for now or 1?
	// User said "minimum 0.50 quantity" previously, implying stepSize 0.01
	// Let's stick to stepSize 0.01 for qty as per previous instruction, but fix price to 3 decimals.
	
	s.quantityPrecision = 2
	s.pricePrecision = 3
	s.stepSize = 0.01
	s.tickSize = 0.001
	s.minQty = 0.01 // Default fallback
	
	utils.Logger.Info("Symbol Info Initialized (Hardcoded - Pending API impl)", 
		zap.Int("qty_prec", s.quantityPrecision),
		zap.Float64("step_size", s.stepSize),
	)
	return nil
}

func (s *MartingaleStrategy) syncState() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 1. Get Position
	pos, err := s.exchange.GetPosition()
	if err != nil {
		utils.Logger.Error("Failed to sync position", zap.Error(err))
		return
	}
	s.position = pos

	amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
	if math.Abs(amt) > 0 {
		s.currentState = StateInPosition
	} else {
		s.currentState = StateIdle
	}
	
	utils.Logger.Info("State Synced", zap.String("state", string(s.currentState)), zap.Float64("amt", amt))
}

// Event Handlers

func (s *MartingaleStrategy) handleTick(ctx context.Context, event core.Event) error {
	price, ok := event.Data.(float64)
	if !ok {
		return fmt.Errorf("invalid tick data")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Logic based on state
	switch s.currentState {
	case StateIdle:
		// If idle, check if we should enter (e.g., immediate entry or signal)
		// For simplicity, let's say we enter immediately if idle
		return s.enterLong(price)
	case StateInPosition:
		// Monitor PnL, check if grid orders are in place (auditing)
		// This is handled mostly by OrderUpdate, but we can do safety checks here
	}
	return nil
}

func (s *MartingaleStrategy) handleOrderUpdate(ctx context.Context, event core.Event) error {
	// The event data from binance.go is *futures.WsOrderTradeUpdate
	// Let's assert it correctly
	order, ok := event.Data.(*futures.WsOrderTradeUpdate)
	if !ok {
		// Try value type if pointer assertion fails, though binance.go sends pointer
		// Or maybe it's wrapped in something else?
		// Let's debug what we got
		return fmt.Errorf("invalid order update data: expected *futures.WsOrderTradeUpdate, got %T", event.Data)
	}

	utils.Logger.Info("Order Update Received", 
		zap.Int64("id", order.ID), 
		zap.String("status", string(order.Status)),
		zap.String("type", string(order.Type)),
	)

	if order.Status == futures.OrderStatusTypeFilled {
		if order.Side == futures.SideTypeBuy {
			// Buy Order Filled (Base or Safety)
			utils.Logger.Info("Buy Order Filled", zap.String("type", string(order.Type)))
			
			s.mu.Lock()
			prevState := s.currentState
			s.currentState = StateInPosition
			s.mu.Unlock()

			if prevState == StateIdle || prevState == StatePlacingGrid {
				// Base order filled -> Place Grid
				go s.placeGridOrders()
			} else {
				// Safety order filled -> Update TP
				utils.Logger.Info("Safety Order Filled. Re-calculating TP.")
				go s.updateTP()
			}
		} else if order.Side == futures.SideTypeSell {
			// Sell Order Filled (TP, Manual, or Stop)
			// Assume any sell fill in Long strategy means closing/reducing position
			// For simplicity in Martingale, we assume full close on TP
			
			utils.Logger.Info("Sell Order Filled (TP/Manual). Resetting to IDLE.", 
				zap.String("type", string(order.Type)),
				zap.String("status", string(order.Status)),
			)
			
			s.mu.Lock()
			s.currentState = StateIdle
			s.currentTPOrderID = 0
			s.mu.Unlock()
			
			s.exchange.CancelAllOrders()
			// Wait a bit before next cycle
			time.Sleep(5 * time.Second)
		}
	}
	return nil
}

// Actions

func (s *MartingaleStrategy) enterLong(currentPrice float64) error {
	utils.Logger.Info("Entering Long Position...")
	
	// Update ATR before entry
	s.updateATR()
	
	// Calculate Base Quantity
	// Logic: MinNotional (5 USDT) / Price -> rounded UP to stepSize
	minNotionalQty := MinNotional / currentPrice
	baseQty := utils.RoundUpToTickSize(minNotionalQty, s.stepSize)
	
	// Ensure baseQty >= minQty
	if baseQty < s.minQty {
		baseQty = s.minQty
	}
	
	// Use configured BaseOrderSize if it's larger than min required
	// But user said: "Start with min notional (5U), then Fibonacci"
	// So we ignore cfg.BaseOrderSize for quantity calc if the intent is strictly 5U start?
	// User said: "calculate base仓位 based on Binance min order size (5U)"
	// Let's use the calculated 5U qty as unit "1" for Fibonacci.
	
	utils.Logger.Info("Calculated Base Qty", 
		zap.Float64("price", currentPrice), 
		zap.Float64("raw_qty", minNotionalQty), 
		zap.Float64("final_qty", baseQty),
	)

	_, err := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeMarket, baseQty, 0)
	if err != nil {
		utils.Logger.Error("Failed to place base order", zap.Error(err))
		return err
	}
	
	s.currentState = StatePlacingGrid
	return nil
}

func (s *MartingaleStrategy) placeGridOrders() {
	// This should be async or robust
	// 1. Calculate Grid Levels based on ATR
	// 2. Batch Place Orders
	
	// Fetch current entry price (avg price)
	pos, err := s.exchange.GetPosition()
	if err != nil {
		return
	}
	entryPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
	
	s.mu.RLock()
	atr := s.currentATR
	s.mu.RUnlock()

	if atr == 0 {
		atr = entryPrice * 0.01 // Fallback 1%
	}
	
	// Calculate Unit Quantity (Fibonacci 1) based on MinNotional logic
	// We need to know what "1 unit" is. It is the base order size (5U).
	unitQty := utils.RoundUpToTickSize(MinNotional / entryPrice, s.stepSize)
	
	utils.Logger.Info("Placing Grid Orders", zap.Float64("Entry", entryPrice), zap.Float64("ATR", atr), zap.Float64("UnitQty", unitQty))

	// Define Multiplier Sequence (Piecewise Function)
	// [1.0, 1.0, 3.1, 2.1, 4.25, 4.25, 6.5]
	multipliers := []float64{1.0, 1.0, 3.1, 2.1, 4.25, 4.25, 6.5}
	
	currentPriceLevel := entryPrice

	for i := 1; i <= s.cfg.MaxSafetyOrders; i++ {
		// Calculate Price: Based on cumulative distance using multipliers
		// Get multiplier for this step, default to last known or 1.0 if out of bounds
		mult := 1.0
		if i-1 < len(multipliers) {
			mult = multipliers[i-1]
		} else if len(multipliers) > 0 {
			mult = multipliers[len(multipliers)-1]
		}
		
		stepDist := atr * mult
		price := currentPriceLevel - stepDist
		currentPriceLevel = price // Update for next step (relative distance)
		
		// Ensure price precision
		price = utils.ToFixed(price, s.pricePrecision) // Should align to tickSize really
		
		// Fibonacci Volume: Qty = UnitQty * Fib(i)
		volMult := s.getFibonacci(i) // 1, 1, 2, 3...
		qty := unitQty * float64(volMult)
		
		// Round qty to stepSize
		qty = utils.RoundUpToTickSize(qty, s.stepSize)
		
		utils.Logger.Info("Placing Safety Order", 
			zap.Int("index", i),
			zap.Float64("price", price),
			zap.Float64("qty", qty),
			zap.Float64("dist_atr", mult),
		)
		
		_, err := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeLimit, qty, price)
		if err != nil {
			utils.Logger.Error("Failed to place safety order", zap.Int("index", i), zap.Error(err))
		}
	}
	
	// Place Initial TP
	s.updateTP()
}

func (s *MartingaleStrategy) updateTP() {
	// 1. Get updated position
	pos, err := s.exchange.GetPosition()
	if err != nil {
		utils.Logger.Error("Failed to get position for TP update", zap.Error(err))
		return
	}
	
	avgPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
	amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
	
	// If position is closed, we don't need a TP
	if math.Abs(amt) == 0 {
		s.mu.Lock()
		s.currentTPOrderID = 0
		s.mu.Unlock()
		return
	}
	
	s.mu.RLock()
	// Safety check: if state is IDLE, don't update TP (cycle finished)
	if s.currentState == StateIdle {
		s.mu.RUnlock()
		return
	}
	atr := s.currentATR
	oldTPID := s.currentTPOrderID
	s.mu.RUnlock()
	
	// 2. Calculate TP Price: Avg + ATR * 0.8
	tpPrice := avgPrice + (atr * 0.8)
	
	// 3. Cancel old TP
	if oldTPID != 0 {
		utils.Logger.Info("Cancelling old TP", zap.Int64("id", oldTPID))
		if err := s.exchange.CancelOrder(oldTPID); err != nil {
			utils.Logger.Warn("Failed to cancel old TP (might be filled or already canceled)", zap.Error(err))
		}
	}
	
	// 4. Place new TP
	// TP Qty = Full Position
	// Round Price to TickSize
	tpPrice = utils.ToFixed(tpPrice, s.pricePrecision)
	
	utils.Logger.Info("Updating TP", zap.Float64("Price", tpPrice), zap.Float64("Qty", amt))
	
	resp, err := s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeLimit, math.Abs(amt), tpPrice)
	if err != nil {
		utils.Logger.Error("Failed to place TP order", zap.Error(err))
		return
	}

	s.mu.Lock()
	if s.currentState == StateIdle {
		s.mu.Unlock()
		utils.Logger.Info("Cycle finished during TP update, cancelling new TP", zap.Int64("id", resp.OrderID))
		go s.exchange.CancelOrder(resp.OrderID)
		return
	}
	s.currentTPOrderID = resp.OrderID
	s.mu.Unlock()
}

func (s *MartingaleStrategy) updateATR() {
	klines, err := s.exchange.GetKlines(50)
	if err != nil {
		utils.Logger.Error("Failed to get klines", zap.Error(err))
		return
	}
	
	var highs, lows, closes []float64
	for _, k := range klines {
		h, _ := strconv.ParseFloat(k.High, 64)
		l, _ := strconv.ParseFloat(k.Low, 64)
		c, _ := strconv.ParseFloat(k.Close, 64)
		highs = append(highs, h)
		lows = append(lows, l)
		closes = append(closes, c)
	}
	
	s.currentATR = utils.CalculateATR(highs, lows, closes, s.cfg.AtrPeriod)
	utils.Logger.Info("ATR Updated", zap.Float64("ATR", s.currentATR))
}

func (s *MartingaleStrategy) getFibonacci(n int) int {
	if n <= 1 {
		return 1
	}
	a, b := 1, 1
	for i := 2; i <= n; i++ {
		a, b = b, a+b
	}
	return b
}
