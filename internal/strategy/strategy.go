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
	s.quantityPrecision = 2
	s.pricePrecision = 4
	s.stepSize = 0.01
	s.tickSize = 0.0001
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
	order, ok := event.Data.(*futures.WsOrderTradeUpdate)
	if !ok {
		return fmt.Errorf("invalid order update data")
	}

	utils.Logger.Info("Order Update Received", 
		zap.Int64("id", order.ID), 
		zap.String("status", string(order.Status)),
		zap.String("type", string(order.Type)),
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	if order.Status == futures.OrderStatusTypeFilled {
		if order.Type == futures.OrderTypeMarket { // Base order filled
			s.currentState = StateInPosition
			// Calculate ATR and Place Grid
			go s.placeGridOrders()
		} else if order.Type == futures.OrderTypeLimit {
			// Could be Safety Order or Take Profit
			// If Safety Order filled -> Update TP
			// If TP filled -> Reset to Idle
			
			// We need a way to distinguish. 
			// In a real system, we'd track Order Client ID.
			// Here we assume: if price > entry, it's TP (for Long). If price < entry, it's Safety.
			// Or check if it reduces position (TP) or increases (Safety).
			
			if order.Side == futures.SideTypeSell { // TP Filled (assuming Long strategy)
				utils.Logger.Info("Take Profit Filled! Cycle Complete.")
				s.currentState = StateIdle
				s.exchange.CancelAllOrders()
				// Wait a bit before next cycle
				time.Sleep(5 * time.Second)
			} else { // Safety Order Filled
				utils.Logger.Info("Safety Order Filled. Re-calculating TP.")
				go s.updateTP()
			}
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
	
	atr := s.currentATR
	if atr == 0 {
		atr = entryPrice * 0.01 // Fallback 1%
	}
	
	// Calculate Unit Quantity (Fibonacci 1) based on MinNotional logic
	// We need to know what "1 unit" is. It is the base order size (5U).
	unitQty := utils.RoundUpToTickSize(MinNotional / entryPrice, s.stepSize)
	
	utils.Logger.Info("Placing Grid Orders", zap.Float64("Entry", entryPrice), zap.Float64("ATR", atr), zap.Float64("UnitQty", unitQty))

	for i := 1; i <= s.cfg.MaxSafetyOrders; i++ {
		// Calculate Price: Entry - (ATR * Multiplier * i)
		stepDist := atr * s.cfg.AtrMultiplier * float64(i) // Simplified linear step for demo
		price := entryPrice - stepDist
		
		// Ensure price precision
		price = utils.ToFixed(price, s.pricePrecision) // Should align to tickSize really
		
		// Fibonacci Volume: Qty = UnitQty * Fib(i)
		volMult := s.getFibonacci(i) // 1, 1, 2, 3...
		qty := unitQty * float64(volMult)
		
		// Round qty to stepSize
		qty = utils.RoundUpToTickSize(qty, s.stepSize)
		
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
		return
	}
	
	avgPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
	amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
	
	// 2. Calculate TP Price: Avg + ATR
	atr := s.currentATR
	tpPrice := avgPrice + atr
	
	// 3. Cancel old TP (if we track it, or just CancelAll sells)
	// For simplicity, we can't easily cancel just TP without ID tracking.
	// But in this logic, we place TP as a LIMIT SELL.
	// We might need to cancel all open SELLS first.
	
	// 4. Place new TP
	// TP Qty = Full Position
	// Round Price to TickSize
	tpPrice = utils.ToFixed(tpPrice, s.pricePrecision)
	
	utils.Logger.Info("Updating TP", zap.Float64("Price", tpPrice), zap.Float64("Qty", amt))
	s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeLimit, math.Abs(amt), tpPrice)
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
