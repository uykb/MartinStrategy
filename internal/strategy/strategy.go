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
	"github.com/uykb/MartinStrategy/internal/utils"
	"go.uber.org/zap"
)

// State definition
type State string

const (
	StateIdle         State = "IDLE"
	StateWaitingEntry State = "WAITING_ENTRY" // 首仓挂单等待成交
	StateInPosition   State = "IN_POSITION"
	StatePlacingGrid  State = "PLACING_GRID"
	StateClosing      State = "CLOSING"
)

// EntryTimeout 首仓挂单超时时间，超时后回退为市价单
const EntryTimeout = 10 * time.Second

// TPCooldown 止盈成交后冷却期，防止快速重入导致反复开平仓
const TPCooldown = 30 * time.Second

// MinOrderValue is the minimum order value in USDT for Binance Futures
const MinOrderValue = 6.0

// safetyOrderMultipliers defines quantity multipliers for each safety order level (1-9).
// Replaces the old Fibonacci/2 scaling. Applied as: qty = unitQty * multiplier.
var safetyOrderMultipliers = []float64{0.03, 0.03, 0.05, 0.05, 0.18, 0.32, 0.567, 0.578, 1.16}

type MartingaleStrategy struct {
	cfg      *config.StrategyConfig
	exchange *exchange.BinanceClient
	bus      *core.EventBus

	mu               sync.RWMutex
	currentState     State
	currentTPOrderID int64
	baseOrderID      int64 // 首仓挂单 ID，用于超时取消

	// Symbol Info
	quantityPrecision int
	pricePrecision    int
	minQty            float64
	stepSize          float64 // For quantity
	tickSize          float64 // For price

	// 防重入锁
	gridMu sync.Mutex // placeGridOrders 防并发
	tpMu   sync.Mutex // updateTP 防并发

	// waitForFillAndPlaceGrid stops when this channel is closed
	waitStopCh chan struct{}

	// 监控计数器
	gridSkipCount int64 // placeGridOrders 跳过次数
	tpSkipCount   int64 // updateTP 跳过次数

	// 状态标志
	gridPlaced      bool  // 标志网格是否已放置，防止重复
	paused          bool  // 策略暂停标志
	gridFilledCount int   // 已成交的网格安全单数量
	lastTPFill      time.Time // 上次止盈成交时间，用于冷却期

	// Dashboard cache (periodically refreshed)
	cachedBalance   float64
	cachedPosition  *futures.AccountPosition
	cachedOrders    []*futures.Order
	cachedMarkPrice float64

	// Dashboard history (ring buffers)
	fills  []FillInfo
	alerts []AlertInfo
}

func NewMartingaleStrategy(cfg *config.StrategyConfig, ex *exchange.BinanceClient, bus *core.EventBus) *MartingaleStrategy {
	return &MartingaleStrategy{
		cfg:          cfg,
		exchange:     ex,
		bus:          bus,
		currentState: StateIdle,
		waitStopCh:   make(chan struct{}),
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

	// Background goroutine to check position status periodically
	// This handles cases where position is closed manually (e.g., via Binance UI)
	go s.monitorPositionStatus()

	// Background cache refresh for web dashboard
	go s.refreshCacheLoop()
}

func (s *MartingaleStrategy) monitorPositionStatus() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.RLock()
		state := s.currentState
		s.mu.RUnlock()

		// Only check when in IN_POSITION state
		if state != StateInPosition {
			continue
		}

		pos, err := s.exchange.GetPosition()
		if err != nil {
			utils.Logger.Error("monitorPositionStatus: failed to get position", zap.Error(err))
			continue
		}

		amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
		if math.Abs(amt) == 0 {
			utils.Logger.Info("monitorPositionStatus: position closed (manually?), resetting state to IDLE")
			s.mu.Lock()
			s.currentState = StateIdle
			s.gridPlaced = false
			s.currentTPOrderID = 0
			s.mu.Unlock()

			// Cancel any remaining orders
			if err := s.exchange.CancelAllOrders(); err != nil {
				utils.Logger.Error("monitorPositionStatus: CancelAllOrders failed", zap.Error(err))
			}
		}
	}
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

	amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
	if math.Abs(amt) > 0 {
		s.currentState = StateInPosition
		s.gridPlaced = true // 如果有持仓，说明网格已放置
		utils.Logger.Info("State Synced (Has Position)", zap.String("state", string(s.currentState)), zap.Float64("amt", amt))

		// If in position, we MUST ensure we have a TP order.
		// Since we might have restarted, our memory (currentTPOrderID) is lost.

		// Check Open Orders
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
		s.gridPlaced = false
		s.currentTPOrderID = 0
		utils.Logger.Info("State Synced (No Position)", zap.String("state", string(s.currentState)))
	}
}

// Event Handlers

func (s *MartingaleStrategy) handleTick(ctx context.Context, event core.Event) error {
	price, ok := event.Data.(float64)
	if !ok {
		return fmt.Errorf("invalid tick data")
	}

	utils.Logger.Info("Tick received", zap.Float64("price", price), zap.String("state", string(s.currentState)), zap.Bool("gridPlaced", s.gridPlaced))

	// 原子状态检查
	s.mu.Lock()
	if s.currentState != StateIdle || s.paused {
		s.mu.Unlock()
		return nil
	}
	// TP 成交冷却期：防止快速重入导致反复开平仓
	if time.Since(s.lastTPFill) < TPCooldown {
		utils.Logger.Info("Tick received during TP cooldown, skipping",
			zap.Duration("since_tp", time.Since(s.lastTPFill)))
		s.mu.Unlock()
		return nil
	}
	utils.Logger.Info("State is IDLE, starting new entry sequence")
	s.currentState = StateWaitingEntry
	s.gridPlaced = false // 重置网格标志

	// 关闭旧的 waitForFillAndPlaceGrid，启动新的
	if s.waitStopCh != nil {
		close(s.waitStopCh)
	}
	s.waitStopCh = make(chan struct{})
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

	// 等待订单成交，然后放置网格
	// 每2秒检查一次，最多等待30秒
	go s.waitForFillAndPlaceGrid()

	return nil
}

func (s *MartingaleStrategy) waitForFillAndPlaceGrid() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeout := time.After(30 * time.Second)

	for {
		select {
		case <-s.waitStopCh:
			utils.Logger.Info("waitForFillAndPlaceGrid: stopped via channel")
			return
		case <-timeout:
			utils.Logger.Warn("waitForFillAndPlaceGrid: timeout, no position found")
			s.mu.Lock()
			s.currentState = StateIdle
			s.mu.Unlock()
			return
		case <-ticker.C:
			s.mu.RLock()
			state := s.currentState
			s.mu.RUnlock()

			if state != StateWaitingEntry && state != StatePlacingGrid {
				utils.Logger.Info("waitForFillAndPlaceGrid: state changed, aborting",
					zap.String("state", string(state)))
				return
			}

			pos, err := s.exchange.GetPosition()
			if err != nil {
				utils.Logger.Error("Failed to get position", zap.Error(err))
				continue
			}

			amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			if math.Abs(amt) > 0 {
				entryPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
				utils.Logger.Info("Position detected, placing grid orders",
					zap.Float64("amt", amt),
					zap.Float64("entryPrice", entryPrice))
				s.placeGridOrders(entryPrice)
				return
			}
		}
	}
}

func (s *MartingaleStrategy) handleOrderUpdate(ctx context.Context, event core.Event) error {
	order, ok := event.Data.(*futures.WsOrderTradeUpdate)
	if !ok {
		utils.Logger.Error("Invalid order update data",
			zap.String("type", fmt.Sprintf("%T", event.Data)))
		return fmt.Errorf("invalid order update data: expected *futures.WsOrderTradeUpdate, got %T", event.Data)
	}

	// 只处理配置的交易对订单
	configuredSymbol := s.exchange.GetSymbol()
	if order.Symbol != configuredSymbol {
		utils.Logger.Debug("Ignoring order update for different symbol",
			zap.String("order_symbol", order.Symbol),
			zap.String("configured_symbol", configuredSymbol))
		return nil
	}

	utils.Logger.Info("Order Update Received",
		zap.Int64("id", order.ID),
		zap.String("status", string(order.Status)),
		zap.String("side", string(order.Side)),
		zap.String("type", string(order.Type)),
	)

	if order.Status == futures.OrderStatusTypeFilled {
		if order.Side == futures.SideTypeBuy {
			buyFilledPrice, _ := strconv.ParseFloat(order.AveragePrice, 64)
			buyFilledQty, _ := strconv.ParseFloat(order.LastFilledQty, 64)
			utils.Logger.Info("Buy Order Filled", zap.String("type", string(order.Type)), zap.Float64("execPrice", buyFilledPrice))

			s.mu.Lock()
			prevState := s.currentState
			s.mu.Unlock()

			s.mu.RLock()
			gridPlaced := s.gridPlaced
			s.mu.RUnlock()

			if prevState == StateIdle || prevState == StateWaitingEntry || prevState == StatePlacingGrid {
				if !gridPlaced {
					utils.Logger.Info("Base order filled, placing grid orders", zap.Float64("execPrice", buyFilledPrice))
					s.addFill("BUY", "BASE", buyFilledPrice, buyFilledQty)
					s.mu.Lock()
					s.currentState = StateInPosition
					s.mu.Unlock()
					go s.placeGridOrders(buyFilledPrice)
				} else {
					utils.Logger.Info("Base order filled but grid already placed, updating TP", zap.Float64("execPrice", buyFilledPrice))
					s.addFill("BUY", "SAFETY", buyFilledPrice, buyFilledQty)
					s.mu.Lock()
					s.gridFilledCount++
					s.currentState = StateInPosition
					s.mu.Unlock()
					go s.updateTP()
				}
			} else {
				utils.Logger.Info("Safety order filled, re-calculating TP", zap.Float64("execPrice", buyFilledPrice))
				s.addFill("BUY", "SAFETY", buyFilledPrice, buyFilledQty)
				s.mu.Lock()
				s.gridFilledCount++
				s.mu.Unlock()
				go s.updateTP()
			}
		} else if order.Side == futures.SideTypeSell {
			sellFilledPrice, _ := strconv.ParseFloat(order.AveragePrice, 64)
			sellFilledQty, _ := strconv.ParseFloat(order.LastFilledQty, 64)

			utils.Logger.Info("Sell Order Filled (TP/Manual). Resetting to IDLE.",
				zap.String("type", string(order.Type)),
				zap.String("status", string(order.Status)),
			)

			s.addFill("SELL", "TP", sellFilledPrice, sellFilledQty)

			s.mu.Lock()
			s.currentState = StateIdle
			s.currentTPOrderID = 0
			s.baseOrderID = 0
			s.gridPlaced = false
			s.gridFilledCount = 0
			s.lastTPFill = time.Now()
			utils.Logger.Info("Sell filled: state reset to IDLE", zap.Bool("gridPlaced", s.gridPlaced))
			s.mu.Unlock()

			// 撤单并重试一次，防止旧委托残留导致下一轮异常
			if err := s.exchange.CancelAllOrders(); err != nil {
				utils.Logger.Error("CancelAllOrders failed after TP fill, retrying", zap.Error(err))
				time.Sleep(500 * time.Millisecond)
				if err2 := s.exchange.CancelAllOrders(); err2 != nil {
					utils.Logger.Error("CancelAllOrders retry also failed", zap.Error(err2))
				} else {
					utils.Logger.Info("CancelAllOrders succeeded on retry")
				}
			} else {
				utils.Logger.Info("All orders cancelled after sell filled")
			}
		}
	}
	return nil
}

// Actions

func (s *MartingaleStrategy) enterLong(currentPrice float64) error {
	utils.Logger.Info("Entering Long Position...")

	// Calculate Base Quantity
	// Logic: Unit = MinNotional (5 USDT) / Price -> rounded UP to stepSize
	// Base Order = 1 * Unit (1倍)
	minNotional := s.calcMinNotional()
	unitQtyRaw := minNotional / currentPrice
	unitQty := utils.RoundUpToTickSize(unitQtyRaw, s.stepSize)

	if unitQty < s.minQty {
		unitQty = s.minQty
	}

	baseQty := unitQty * 1.0
	baseQty = utils.ToFixed(baseQty, s.quantityPrecision)

	// 挂单价格：currentPrice + 2*tickSize，略高于当前价以提高成交概率
	limitPrice := currentPrice + 2*s.tickSize
	limitPrice = utils.RoundToTickSize(limitPrice, s.tickSize)
	limitPrice = utils.ToFixed(limitPrice, s.pricePrecision)

	utils.Logger.Info("Calculated Base Qty (Maker Limit)",
		zap.Float64("price", currentPrice),
		zap.Float64("limit_price", limitPrice),
		zap.Float64("unit_qty", unitQty),
		zap.Float64("base_qty", baseQty),
	)

	// 尝试挂限价单（Maker 费率 0.02% vs Taker 0.05%）
	resp, err := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeLimit, baseQty, limitPrice, false)
	if err != nil {
		utils.Logger.Error("Failed to place base limit order, falling back to market", zap.Error(err))
		// 挂单失败直接回退市价
		_, err2 := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeMarket, baseQty, 0, false)
		if err2 != nil {
			utils.Logger.Error("Failed to place base market order", zap.Error(err2))
			return err2
		}
		return nil
	}

	// 记录首仓挂单 ID
	s.mu.Lock()
	s.baseOrderID = resp.OrderID
	s.mu.Unlock()

	utils.Logger.Info("Base limit order placed",
		zap.Int64("order_id", resp.OrderID),
		zap.Float64("limit_price", limitPrice),
		zap.Float64("qty", baseQty),
	)

	// 启动超时 goroutine：超时未成交则撤单并回退市价
	go s.waitForEntryTimeout(baseQty)

	return nil
}

// waitForEntryTimeout 首仓挂单超时监控
// 如果 EntryTimeout 内首仓限价单未成交，撤单并以市价单重新入场
func (s *MartingaleStrategy) waitForEntryTimeout(baseQty float64) {
	// 快照当前 baseOrderID，防止跨周期误判
	s.mu.RLock()
	myOrderID := s.baseOrderID
	s.mu.RUnlock()

	timer := time.NewTimer(EntryTimeout)
	defer timer.Stop()

	select {
	case <-s.waitStopCh:
		// 新一轮入场开始，旧的超时监控退出
		utils.Logger.Info("waitForEntryTimeout: stopped via channel")
		return
	case <-timer.C:
		// 超时，检查状态是否仍在等待入场且 baseOrderID 未变更
		s.mu.RLock()
		state := s.currentState
		currentOrderID := s.baseOrderID
		s.mu.RUnlock()

		if state != StateWaitingEntry || currentOrderID != myOrderID {
			utils.Logger.Info("waitForEntryTimeout: state or order changed, aborting",
				zap.String("state", string(state)),
				zap.Int64("my_order", myOrderID),
				zap.Int64("current_order", currentOrderID))
			return
		}

		utils.Logger.Info("waitForEntryTimeout: limit order not filled in time, cancelling",
			zap.Int64("order_id", myOrderID))

		// 撤销限价单
		if err := s.exchange.CancelOrder(myOrderID); err != nil {
			utils.Logger.Warn("waitForEntryTimeout: failed to cancel limit order (may already be filled)",
				zap.Error(err))
			// 撤单失败可能是已成交，检查持仓确认
			pos, err := s.exchange.GetPosition()
			if err == nil {
				amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
				if math.Abs(amt) > 0 {
					utils.Logger.Info("waitForEntryTimeout: position exists, limit order was filled")
					return
				}
			}
			// 无法确认，仍然尝试市价回退
		}

		// 再次检查状态（可能在撤单过程中收到了成交事件）
		s.mu.RLock()
		state = s.currentState
		currentOrderID = s.baseOrderID
		s.mu.RUnlock()
		if state != StateWaitingEntry || currentOrderID != myOrderID {
			utils.Logger.Info("waitForEntryTimeout: state or order changed after cancel, aborting",
				zap.String("state", string(state)))
			return
		}

		// 回退市价单
		utils.Logger.Info("waitForEntryTimeout: placing market fallback order")
		_, err := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeMarket, baseQty, 0, false)
		if err != nil {
			utils.Logger.Error("waitForEntryTimeout: market fallback failed", zap.Error(err))
			s.mu.Lock()
			s.currentState = StateIdle
			s.mu.Unlock()
		}
	}
}

func (s *MartingaleStrategy) placeGridOrders(execPrice float64) {
	utils.Logger.Info("placeGridOrders started", zap.Float64("execPrice", execPrice))

	// 检查网格是否已放置，防止重复
	s.mu.RLock()
	if s.gridPlaced {
		s.mu.RUnlock()
		utils.Logger.Warn("placeGridOrders skipped: grid already placed")
		return
	}
	s.mu.RUnlock()

	// 检查是否已有活跃的网格订单（防止重启后重复放置）
	existingOrders, err := s.exchange.GetOpenOrders()
	if err == nil && len(existingOrders) > 0 {
		// 计算除了 TP 以外的网格订单数量
		gridCount := 0
		for _, o := range existingOrders {
			if o.Side == futures.SideTypeBuy {
				gridCount++
			}
		}
		if gridCount > 0 {
			utils.Logger.Warn("placeGridOrders skipped: existing grid orders found",
				zap.Int("existing_grid_count", gridCount),
				zap.Int("total_orders", len(existingOrders)))
			// 更新 gridPlaced 标记为 true
			s.mu.Lock()
			s.gridPlaced = true
			s.mu.Unlock()
			return
		}
	}

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

	// 再次检查（获取锁后）
	s.mu.RLock()
	if s.gridPlaced {
		s.mu.RUnlock()
		utils.Logger.Warn("placeGridOrders skipped: grid already placed (after lock)")
		return
	}
	s.mu.RUnlock()

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

	minNotional := s.calcMinNotional()

	unitQty := utils.RoundUpToTickSize(minNotional/entryPrice, s.stepSize)

	utils.Logger.Info("Placing Grid Orders", zap.Float64("Entry", entryPrice), zap.Float64("UnitQty", unitQty))

	// Fixed percentage-based grid distances, relative to previous level
	// Level 1-9: 1.0%, 1.0%, 1.0%, 1.1%, 2.1%, 2.2%, 4.5%, 4.8%, 7.7%
	gridPcts := []float64{1.0, 1.0, 1.0, 1.1, 2.1, 2.2, 4.5, 4.8, 7.7}

	currentPriceLevel := entryPrice

	for i := 0; i < len(gridPcts); i++ {
		// Price = previous level * (1 - pct/100)
		stepPct := gridPcts[i]
		price := currentPriceLevel * (1 - stepPct/100)
		currentPriceLevel = price

		// Ensure price precision
		price = utils.RoundToTickSize(price, s.tickSize)
		price = utils.ToFixed(price, s.pricePrecision) // Should align to tickSize really

		// Safety order multipliers (fixed, replaces old Fibonacci/2 scaling)
		volMult := safetyOrderMultipliers[i]
		qty := unitQty * volMult

		// Ensure MinNotional (5 USDT) at the LIMIT PRICE
		// If Qty * Price < 5.0, Binance will reject.
		// Since Price < EntryPrice, the original UnitQty (based on EntryPrice) might be insufficient.
		if qty*price < minNotional {
			utils.Logger.Info("Adjusting Qty to meet MinNotional",
				zap.Int("index", i+1),
				zap.Float64("old_qty", qty),
				zap.Float64("price", price),
			)
			qty = minNotional / price
		}

		// Round qty to stepSize
		qty = utils.RoundUpToTickSize(qty, s.stepSize)
		qty = utils.ToFixed(qty, s.quantityPrecision)

		utils.Logger.Info("Placing Safety Order",
			zap.Int("index", i+1),
			zap.Float64("price", price),
			zap.Float64("qty", qty),
			zap.Float64("dist_pct", stepPct),
		)

		_, err := s.exchange.PlaceOrder(futures.SideTypeBuy, futures.OrderTypeLimit, qty, price, false)
		if err != nil {
			utils.Logger.Error("Failed to place safety order", zap.Int("index", i), zap.Error(err))
		}

		// Avoid hitting API rate limits
		time.Sleep(200 * time.Millisecond)
	}

	// Place Initial TP
	s.updateTP()

	// 标记网格已放置
	s.mu.Lock()
	s.gridPlaced = true
	s.mu.Unlock()
	utils.Logger.Info("Grid orders placed successfully, gridPlaced=true")
}

func (s *MartingaleStrategy) updateTP() {
	utils.Logger.Info("updateTP started")

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

	utils.Logger.Info("updateTP acquired lock")

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
	// TP = average entry price + 0.80%
	tpPrice := avgPrice * 1.008
	oldTPID := s.currentTPOrderID
	s.mu.RUnlock()

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

	resp, err := s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeLimit, tpQty, tpPrice, true)
	if err != nil {
		utils.Logger.Warn("Failed to place TP order, retrying once", zap.Error(err))
		time.Sleep(500 * time.Millisecond)
		resp, err = s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeLimit, tpQty, tpPrice, true)
		if err != nil {
			utils.Logger.Error("Failed to place TP order after retry", zap.Error(err))
			return
		}
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

func (s *MartingaleStrategy) calcMinNotional() float64 {
	balance, err := s.exchange.GetBalance()
	if err != nil {
		utils.Logger.Error("Failed to get balance, using MinOrderValue", zap.Error(err))
		return MinOrderValue
	}
	notional := balance * s.cfg.BaseRatio
	if notional < MinOrderValue {
		notional = MinOrderValue
	}
	utils.Logger.Info("Dynamic MinNotional", zap.Float64("balance", balance), zap.Float64("ratio", s.cfg.BaseRatio), zap.Float64("notional", notional))
	return notional
}


