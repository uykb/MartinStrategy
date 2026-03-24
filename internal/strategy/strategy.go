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
const MinNotional = 50.0

type MartingaleStrategy struct {
	cfg      *config.StrategyConfig
	exchange *exchange.BinanceClient
	storage  *storage.Database
	bus      *core.EventBus

	mu               sync.RWMutex
	currentState     State
	position         *futures.AccountPosition
	activeOrders     map[int64]*futures.Order // Local cache of active orders
	currentTPOrderID int64

	currentATR float64

	// Symbol Info
	quantityPrecision int
	pricePrecision    int
	minQty            float64
	stepSize          float64 // For quantity
	tickSize          float64 // For price

	// 防重入锁
	gridMu sync.Mutex // placeGridOrders 防并发
	tpMu   sync.Mutex // updateTP 防并发

	// 监控计数器
	gridSkipCount int64 // placeGridOrders 跳过次数
	tpSkipCount   int64 // updateTP 跳过次数
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
	info, err := s.exchange.GetExchangeInfo()
	if err != nil {
		return fmt.Errorf("failed to get exchange info: %w", err)
	}

	symbol := s.exchange.GetSymbol()
	var symbolInfo futures.Symbol
	found := false
	for _, sym := range info.Symbols {
		if sym.Symbol == symbol {
			symbolInfo = sym
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("symbol %s not found in exchange info", symbol)
	}

	s.quantityPrecision = symbolInfo.QuantityPrecision
	s.pricePrecision = symbolInfo.PricePrecision

	// Parse Filters
	for _, filter := range symbolInfo.Filters {
		filterType, ok := filter["filterType"].(string)
		if !ok {
			continue
		}

		switch filterType {
		case "LOT_SIZE":
			if stepSize, ok := filter["stepSize"].(string); ok {
				s.stepSize, _ = strconv.ParseFloat(stepSize, 64)
			}
			if minQty, ok := filter["minQty"].(string); ok {
				s.minQty, _ = strconv.ParseFloat(minQty, 64)
			}
		case "PRICE_FILTER":
			if tickSize, ok := filter["tickSize"].(string); ok {
				s.tickSize, _ = strconv.ParseFloat(tickSize, 64)
			}
		}
	}

	utils.Logger.Info("Symbol Info Initialized",
		zap.String("symbol", symbol),
		zap.Int("price_prec", s.pricePrecision),
		zap.Int("qty_prec", s.quantityPrecision),
		zap.Float64("step_size", s.stepSize),
		zap.Float64("tick_size", s.tickSize),
		zap.Float64("min_qty", s.minQty),
	)
	return nil
}

func (s *MartingaleStrategy) syncState() {
	// Note: We avoid holding s.mu.Lock() for the entire duration if we do heavy network calls
	// But syncState is initialization, so it's fine.

	// 1. Get Position (Network call, could be outside lock, but we need atomic update)
	// Let's do it inside for simplicity as it's init.

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

		// If in position, we MUST ensure we have a TP order.
		// Since we might have restarted, our memory (currentTPOrderID) is lost.

		// 1. Update ATR (Critical for TP calculation)
		// Note: updateATR makes a network call. Inside Lock it blocks, but for init it's acceptable.
		s.updateATR()

		// 2. Check Open Orders
		orders, err := s.exchange.GetOpenOrders()
		if err != nil {
			utils.Logger.Error("Failed to get open orders", zap.Error(err))
		} else {
			hasTP := false
			// Simple check: do we have any Sell Limit orders?
			// In a complex bot, we'd check ClientOrderID or Metadata.
			for _, o := range orders {
				if o.Side == futures.SideTypeSell && o.Type == futures.OrderTypeLimit {
					hasTP = true
					s.currentTPOrderID = o.OrderID
					utils.Logger.Info("Found existing TP order", zap.Int64("id", o.OrderID))
					break
				}
			}

			if !hasTP {
				utils.Logger.Warn("Detected position without TP order. Restoring TP...")
				// We launch this in a goroutine to avoid deadlock if updateTP needs lock (it does RLock)
				// But wait, updateTP needs RLock, we hold Lock. Deadlock!
				// We must release lock before calling updateTP, or updateTP shouldn't lock if called internally.
				// Better: Release lock, then call updateTP.

				// But we are in defer s.mu.Unlock().
				// Let's use a flag and do it after unlock?
				// Or spawn a goroutine that waits a bit?
				// safest: spawn goroutine.
				go func() {
					// Wait a tiny bit for this lock to release
					time.Sleep(100 * time.Millisecond)
					s.updateTP()
				}()
			} else {
				// If we have TP, we might also want to restore Grid Orders if they are missing?
				// For now, let's just log.
				utils.Logger.Info("State restored with existing TP.", zap.Int("open_orders", len(orders)))
			}
		}

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

	// 原子状态检查
	s.mu.Lock()
	if s.currentState != StateIdle {
		s.mu.Unlock()
		return nil
	}
	s.currentState = StatePlacingGrid
	s.mu.Unlock()

	// 网络请求在锁外执行
	if err := s.enterLong(price); err != nil {
		// 下单失败，恢复状态
		s.mu.Lock()
		s.currentState = StateIdle
		s.mu.Unlock()
		utils.Logger.Error("enterLong failed, resetting to IDLE", zap.Error(err))
		return err
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
				// Get execution price from order event to avoid race condition with position API
				execPrice, _ := strconv.ParseFloat(order.AveragePrice, 64)
				go s.placeGridOrders(execPrice)
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
			time.Sleep(10 * time.Second)
		}
	}
	return nil
}

// Actions

func (s *MartingaleStrategy) enterLong(currentPrice float64) error {
	utils.Logger.Info("Entering Long Position...")

	// Update ATR before entry (network call, no lock held)
	s.updateATR()

	// Calculate Base Quantity
	// Logic: Unit = MinNotional (5 USDT) / Price -> rounded UP to stepSize
	// Base Order = 1 * Unit (1倍)
	unitQtyRaw := MinNotional / currentPrice
	unitQty := utils.RoundUpToTickSize(unitQtyRaw, s.stepSize)

	if unitQty < s.minQty {
		unitQty = s.minQty
	}

	baseQty := unitQty * 1.0
	baseQty = utils.ToFixed(baseQty, s.quantityPrecision)

	utils.Logger.Info("Calculated Base Qty",
		zap.Float64("price", currentPrice),
		zap.Float64("unit_qty", unitQty),
		zap.Float64("base_qty", baseQty),
	)

	_, err := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeMarket, baseQty, 0)
	if err != nil {
		utils.Logger.Error("Failed to place base order", zap.Error(err))
		return err
	}

	// 状态已在 handleTick 中设置为 StatePlacingGrid
	return nil
}

func (s *MartingaleStrategy) placeGridOrders(execPrice float64) {
	// 防并发：如果已有实例在执行则跳过
	if !s.gridMu.TryLock() {
		s.mu.Lock()
		s.gridSkipCount++
		skipCount := s.gridSkipCount
		s.mu.Unlock()
		utils.Logger.Warn("placeGridOrders skipped: already running",
			zap.Int64("skip_count", skipCount))
		return
	}
	defer s.gridMu.Unlock()

	// This should be async or robust
	// 1. Calculate Grid Levels based on ATR
	// 2. Batch Place Orders

	var entryPrice float64

	// Use execution price from order event if available (avoids race condition)
	if execPrice > 0 {
		entryPrice = execPrice
		utils.Logger.Info("Using execution price from order event", zap.Float64("entryPrice", entryPrice))
	} else {
		// Fallback: Fetch from position API
		pos, err := s.exchange.GetPosition()
		if err != nil {
			utils.Logger.Error("Failed to get position for grid orders", zap.Error(err))
			return
		}
		entryPrice, _ = strconv.ParseFloat(pos.EntryPrice, 64)
		utils.Logger.Info("Using entry price from position API", zap.Float64("entryPrice", entryPrice))
	}

	// Validate entry price
	if entryPrice <= 0 {
		utils.Logger.Error("Invalid entry price, cannot place grid orders", zap.Float64("entryPrice", entryPrice))
		return
	}

	// Pre-calculate ATRs for different timeframes
	atr30m := s.fetchATR("30m")
	atr1h := s.fetchATR("1h")
	atr2h := s.fetchATR("2h")
	atr4h := s.fetchATR("4h")
	atr6h := s.fetchATR("6h")
	atr8h := s.fetchATR("8h")
	atr12h := s.fetchATR("12h")
	atr1d := s.fetchATR("1d")

	// If any ATR failed (0), fallback to entryPrice * 0.01
	if atr30m == 0 {
		atr30m = entryPrice * 0.01
	}
	if atr1h == 0 {
		atr1h = entryPrice * 0.01
	}
	if atr2h == 0 {
		atr2h = entryPrice * 0.01
	}
	if atr4h == 0 {
		atr4h = entryPrice * 0.01
	}
	if atr6h == 0 {
		atr6h = entryPrice * 0.01
	}
	if atr8h == 0 {
		atr8h = entryPrice * 0.01
	}
	if atr12h == 0 {
		atr12h = entryPrice * 0.01
	}
	if atr1d == 0 {
		atr1d = entryPrice * 0.01
	}

	// Calculate Unit Quantity (Fibonacci 1) based on MinNotional logic
	// We need to know what "1 unit" is. It is the base order size (5U).
	unitQty := utils.RoundUpToTickSize(MinNotional/entryPrice, s.stepSize)

	utils.Logger.Info("Placing Grid Orders", zap.Float64("Entry", entryPrice), zap.Float64("ATR30m", atr30m), zap.Float64("UnitQty", unitQty))

	// Define Multiplier Sequence (Piecewise Function)
	// 1: 30m, 2: 30m, 3: 1h, 4: 2h, 5: 4h, 6: 6h, 7: 8h, 8: 12h, 9: 1D
	// Distances are relative to previous order
	gridDistances := []float64{
		atr30m, // 1
		atr30m, // 2
		atr1h,  // 3
		atr2h,  // 4
		atr4h,  // 5
		atr6h,  // 6
		atr8h,  // 7
		atr12h, // 8
		atr1d,  // 9
	}

	currentPriceLevel := entryPrice

	for i := 1; i <= s.cfg.MaxSafetyOrders; i++ {
		// Calculate Price: Based on cumulative distance
		stepDist := 0.0
		if i-1 < len(gridDistances) {
			stepDist = gridDistances[i-1]
		} else {
			// Fallback to last known distance if config has more orders than we defined
			stepDist = gridDistances[len(gridDistances)-1]
		}

		price := currentPriceLevel - stepDist
		currentPriceLevel = price // Update for next step (relative distance)

		// Ensure price precision
		price = utils.RoundToTickSize(price, s.tickSize)
		price = utils.ToFixed(price, s.pricePrecision) // Should align to tickSize really

		// Fibonacci Volume: Qty = UnitQty * Fib(i)
		volMult := s.getFibonacci(i) // 1, 2, 3, 5...
		qty := unitQty * float64(volMult)

		// Ensure MinNotional (5 USDT) at the LIMIT PRICE
		// If Qty * Price < 5.0, Binance will reject.
		// Since Price < EntryPrice, the original UnitQty (based on EntryPrice) might be insufficient.
		if qty*price < MinNotional {
			utils.Logger.Info("Adjusting Qty to meet MinNotional",
				zap.Int("index", i),
				zap.Float64("old_qty", qty),
				zap.Float64("price", price),
			)
			qty = MinNotional / price
		}

		// Round qty to stepSize
		qty = utils.RoundUpToTickSize(qty, s.stepSize)
		qty = utils.ToFixed(qty, s.quantityPrecision)

		utils.Logger.Info("Placing Safety Order",
			zap.Int("index", i),
			zap.Float64("price", price),
			zap.Float64("qty", qty),
			zap.Float64("dist_atr", stepDist),
		)

		_, err := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeLimit, qty, price)
		if err != nil {
			utils.Logger.Error("Failed to place safety order", zap.Int("index", i), zap.Error(err))
		}

		// Avoid hitting API rate limits
		time.Sleep(200 * time.Millisecond)
	}

	// Place Initial TP
	s.updateTP()
}

func (s *MartingaleStrategy) updateTP() {
	// 防并发：如果已有实例在执行则跳过
	if !s.tpMu.TryLock() {
		s.mu.Lock()
		s.tpSkipCount++
		skipCount := s.tpSkipCount
		s.mu.Unlock()
		utils.Logger.Warn("updateTP skipped: already running",
			zap.Int64("skip_count", skipCount))
		return
	}
	defer s.tpMu.Unlock()

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
	// Always use 15m ATR for TP as requested
	atr15m := s.fetchATR("15m")
	if atr15m == 0 {
		atr15m = avgPrice * 0.01
	}
	oldTPID := s.currentTPOrderID
	s.mu.RUnlock()

	tpPrice := avgPrice + atr15m

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
	tpPrice = utils.RoundToTickSize(tpPrice, s.tickSize)
	// Double check with precision just in case
	tpPrice = utils.ToFixed(tpPrice, s.pricePrecision)

	// Round Qty to precision
	tpQty := utils.ToFixed(math.Abs(amt), s.quantityPrecision)

	utils.Logger.Info("Updating TP", zap.Float64("Price", tpPrice), zap.Float64("Qty", tpQty))

	resp, err := s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeLimit, tpQty, tpPrice)
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
	// Deprecated or can be kept as a default updater for s.currentATR if needed elsewhere
	// But since we now fetch specific ATRs on demand, we can simplify or remove.
	// For backward compatibility with other parts if they use s.currentATR:
	s.currentATR = s.fetchATR("15m")
	utils.Logger.Info("ATR Updated (Default 15m)", zap.Float64("ATR", s.currentATR))
}

func (s *MartingaleStrategy) fetchATR(interval string) float64 {
	klines, err := s.exchange.GetKlines(interval, 50)
	if err != nil {
		utils.Logger.Error("Failed to get klines", zap.String("interval", interval), zap.Error(err))
		return 0
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

	return utils.CalculateATR(highs, lows, closes, s.cfg.AtrPeriod)
}

func (s *MartingaleStrategy) getFibonacci(n int) int {
	if n <= 0 {
		return 0
	}
	if n == 1 {
		return 1
	}
	if n == 2 {
		return 2
	}
	a, b := 1, 2
	for i := 3; i <= n; i++ {
		a, b = b, a+b
	}
	return b
}
