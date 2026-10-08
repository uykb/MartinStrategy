package strategy

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
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

// safetyOrderAllocations defines balance allocation percentages for each safety order level (Grid 1-9 / Levels 2-10).
// Level 2 (Grid 1): 3%
// Level 3 (Grid 2): 3%
// Level 4 (Grid 3): 5%
// Level 5 (Grid 4): 5%
// Level 6 (Grid 5): 18%
// Level 7 (Grid 6): 32%
// Level 8 (Grid 7): 56.7%
// Level 9 (Grid 8): 57.8%
// Level 10 (Grid 9): 116%
var safetyOrderAllocations = []float64{0.03, 0.03, 0.05, 0.05, 0.18, 0.32, 0.567, 0.578, 1.16}

type MartingaleStrategy struct {
	cfg      *config.StrategyConfig
	exchange *exchange.BinanceClient
	bus      *core.EventBus

	mu               sync.RWMutex
	currentState     State
	currentTPOrderID int64
	baseOrderID      int64 // 首仓挂单 ID，用于超时取消

	// TP 状态跟踪：用于检测仓位变化，避免无变化时的冗余更新
	lastTPQty   float64
	lastTPPrice float64
	tpDirty     atomic.Bool

	// 周期代际：每次新周期入场递增，防止异步撤单误撤新周期的挂单
	cycleID uint64

	// 生命周期控制
	ctx    context.Context
	cancel context.CancelFunc

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
	gridPlaced      bool      // 标志网格是否已放置，防止重复
	paused          bool      // 策略暂停标志
	gridFilledCount int       // 已成交的网格安全单数量
	lastTPFill      time.Time // 上次止盈成交时间，用于冷却期

	// Dashboard cache (periodically refreshed)
	cachedBalance   float64
	cachedPosition  *futures.AccountPosition
	cachedOrders    []*futures.Order
	cachedMarkPrice float64

	initialEntryPrice float64 // 首仓入场价格

	// Dashboard history (ring buffers)
	fills  []FillInfo
	alerts []AlertInfo
}

func NewMartingaleStrategy(cfg *config.StrategyConfig, ex *exchange.BinanceClient, bus *core.EventBus) *MartingaleStrategy {
	ctx, cancel := context.WithCancel(context.Background())
	return &MartingaleStrategy{
		cfg:          cfg,
		exchange:     ex,
		bus:          bus,
		currentState: StateIdle,
		waitStopCh:   make(chan struct{}),
		ctx:          ctx,
		cancel:       cancel,
	}
}

// Stop 优雅停止策略
func (s *MartingaleStrategy) Stop() {
	if s.cancel != nil {
		s.cancel()
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
	defer func() {
		if r := recover(); r != nil {
			utils.Logger.Error("monitorPositionStatus panic 恢复，5秒后自愈重启", zap.Any("recover", r))
			time.Sleep(5 * time.Second)
			go s.monitorPositionStatus()
		}
	}()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.RLock()
			state := s.currentState
			cid := s.cycleID
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
				utils.Logger.Info("monitorPositionStatus: position closed (manually or TP filled), resetting state to IDLE")
				go s.cleanCycleAndResetToIdle(cid)
			} else {
				// 持仓巡检保护：确保止盈单存在且挂单量 100% 覆盖当前全部持仓
				orders, err := s.exchange.GetOpenOrders()
				if err == nil {
					var validTP *futures.Order
					sellCount := 0
					for _, o := range orders {
						if o.Side == futures.SideTypeSell && o.Type == futures.OrderTypeLimit {
							sellCount++
							validTP = o
						}
					}

					needTPRepair := false
					if sellCount != 1 || validTP == nil {
						needTPRepair = true
					} else {
						tpQty, _ := strconv.ParseFloat(validTP.OrigQuantity, 64)
						// 若 TP 数量与当前持仓总量不一致（差值大于半个步长）
						if math.Abs(tpQty-math.Abs(amt)) > s.stepSize/2 {
							needTPRepair = true
						}
					}

					if needTPRepair {
						utils.Logger.Warn("monitorPositionStatus: TP missing or mismatch with total position, repairing TP immediately",
							zap.Float64("total_position", math.Abs(amt)),
							zap.Int("sell_orders_count", sellCount))
						go s.safeUpdateTP()
					}
				}
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
		// ★ 冷启动安全原则：已有持仓时绝不重新挂网格单，防止叠加杠杆引发爆仓风险
		s.gridPlaced = true
		if s.initialEntryPrice == 0 {
			s.initialEntryPrice, _ = strconv.ParseFloat(pos.EntryPrice, 64)
		}
		utils.Logger.Info("State Synced (Has Position)", zap.String("state", string(s.currentState)), zap.Float64("amt", amt))

		// Check Open Orders
		orders, err := s.exchange.GetOpenOrders()
		if err != nil {
			utils.Logger.Error("Failed to get open orders during sync", zap.Error(err))
		} else {
			var validTPOrder *futures.Order
			var extraTPIDs []int64
			buyCount := 0
			var extraBuyIDs []int64

			for _, o := range orders {
				if o.Side == futures.SideTypeSell && o.Type == futures.OrderTypeLimit {
					tpQty, _ := strconv.ParseFloat(o.OrigQuantity, 64)
					// 严格比对：TP 挂单数量必须与当前总持仓量一致
					if validTPOrder == nil && math.Abs(tpQty-math.Abs(amt)) <= s.stepSize/2 {
						validTPOrder = o
						s.currentTPOrderID = o.OrderID
						s.lastTPQty = tpQty
						tpPrice, _ := strconv.ParseFloat(o.Price, 64)
						s.lastTPPrice = tpPrice
					} else {
						extraTPIDs = append(extraTPIDs, o.OrderID)
					}
				} else if o.Side == futures.SideTypeBuy {
					buyCount++
					extraBuyIDs = append(extraBuyIDs, o.OrderID)
				}
			}

			// 如果有数量不匹配或多余的旧 TP 单，清理它们
			for _, id := range extraTPIDs {
				utils.Logger.Info("Cleaning up mismatched/extra old TP order on sync", zap.Int64("id", id))
				_ = s.exchange.CancelOrder(id)
			}

			// 若 BUY 挂单数异常（如历史重试产生 >9 个），仅清理多余部分，绝不全量撤单后重挂
			if buyCount > len(safetyOrderAllocations) {
				utils.Logger.Warn("Detected abnormal buy orders count on startup, cleaning up excess",
					zap.Int("count", buyCount),
					zap.Int("expected_max", len(safetyOrderAllocations)))
				for i := len(safetyOrderAllocations); i < len(extraBuyIDs); i++ {
					_ = s.exchange.CancelOrder(extraBuyIDs[i])
				}
				s.gridFilledCount = 0
			} else {
				// 正常情况：已成交层数 = 最大层数 - 剩余挂单数
				s.gridFilledCount = len(safetyOrderAllocations) - buyCount
				if s.gridFilledCount < 0 {
					s.gridFilledCount = 0
				}
			}

			// 如果没有完全匹配 100% 持仓量的 TP 单，立即重新挂出最新全额止盈单
			if validTPOrder == nil {
				utils.Logger.Warn("Detected position without matching 100% TP order. Restoring fresh TP...")
				go func() {
					time.Sleep(100 * time.Millisecond)
					s.safeUpdateTP()
				}()
			} else {
				utils.Logger.Info("State restored with valid 100% TP order.",
					zap.Int64("tpOrderID", validTPOrder.OrderID),
					zap.Float64("posAmt", math.Abs(amt)),
					zap.Int("buy_orders", buyCount))
			}
		}

	} else {
		s.currentState = StateIdle
		s.gridPlaced = false
		s.currentTPOrderID = 0
		s.gridFilledCount = 0
		s.lastTPQty = 0
		s.lastTPPrice = 0
		s.initialEntryPrice = 0.0
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

	s.cycleID++
	currentCycle := s.cycleID
	utils.Logger.Info("State is IDLE, starting new entry sequence", zap.Uint64("cycle_id", currentCycle))
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
		if s.cycleID == currentCycle {
			s.currentState = StateIdle
			s.initialEntryPrice = 0.0
		}
		s.mu.Unlock()
		utils.Logger.Error("enterLong failed, resetting to IDLE", zap.Error(err))
		return err
	}

	// 等待订单成交，然后放置网格
	// 每2秒检查一次，最多等待30秒
	go s.waitForFillAndPlaceGrid(currentCycle)

	return nil
}

func (s *MartingaleStrategy) waitForFillAndPlaceGrid(cycleID uint64) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	timeout := time.After(30 * time.Second)

	for {
		select {
		case <-s.waitStopCh:
			utils.Logger.Info("waitForFillAndPlaceGrid: stopped via channel")
			return
		case <-timeout:
			utils.Logger.Warn("waitForFillAndPlaceGrid: timeout, checking position")
			s.mu.RLock()
			gridPlaced := s.gridPlaced
			cid := s.cycleID
			s.mu.RUnlock()
			if !gridPlaced && cid == cycleID {
				s.mu.Lock()
				if s.cycleID == cycleID && !s.gridPlaced {
					s.currentState = StateIdle
					s.initialEntryPrice = 0.0
				}
				s.mu.Unlock()
			}
			return
		case <-ticker.C:
			s.mu.RLock()
			state := s.currentState
			gridPlaced := s.gridPlaced
			cid := s.cycleID
			s.mu.RUnlock()

			// 如果代际不匹配、网格已放置或状态已不是等待进场/布网中，直接退出
			if cid != cycleID || gridPlaced || (state != StateWaitingEntry && state != StatePlacingGrid) {
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
				s.mu.Lock()
				s.currentState = StateInPosition
				s.initialEntryPrice = entryPrice
				s.mu.Unlock()
				s.safePlaceGridOrders(entryPrice)
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

			s.mu.RLock()
			gridPlaced := s.gridPlaced
			baseOrderID := s.baseOrderID
			s.mu.RUnlock()

			// 判断是否为首仓成交（首仓挂单 ID 匹配，或网格尚未放置）
			if !gridPlaced || (baseOrderID != 0 && order.ID == baseOrderID) {
				utils.Logger.Info("Base order filled, placing grid orders", zap.Float64("execPrice", buyFilledPrice))
				s.addFill("BUY", "BASE", buyFilledPrice, buyFilledQty)
				s.mu.Lock()
				s.baseOrderID = 0
				s.initialEntryPrice = buyFilledPrice
				s.currentState = StateInPosition
				s.mu.Unlock()
				go s.safePlaceGridOrders(buyFilledPrice)
			} else {
				utils.Logger.Info("Safety order filled, re-calculating TP", zap.Float64("execPrice", buyFilledPrice))
				s.addFill("BUY", "SAFETY", buyFilledPrice, buyFilledQty)
				s.mu.Lock()
				if s.gridFilledCount < len(safetyOrderAllocations) {
					s.gridFilledCount++
				}
				s.currentState = StateInPosition
				s.mu.Unlock()
				go s.safeUpdateTP()
			}
		} else if order.Side == futures.SideTypeSell {
			sellFilledPrice, _ := strconv.ParseFloat(order.AveragePrice, 64)
			sellFilledQty, _ := strconv.ParseFloat(order.LastFilledQty, 64)

			utils.Logger.Info("Sell Order Filled (TP/Manual). Waiting for position zero before resetting to IDLE",
				zap.String("type", string(order.Type)),
				zap.String("status", string(order.Status)),
			)

			s.addFill("SELL", "TP", sellFilledPrice, sellFilledQty)

			// ★ 轮询确认持仓真正归零后再重置为 IDLE，防止部分成交或 API 延迟过早开启新周期
			s.mu.RLock()
			cid := s.cycleID
			s.mu.RUnlock()
			go s.waitPositionZeroThenReset(cid)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// 周期清理与归零保护
// ---------------------------------------------------------------------------

func (s *MartingaleStrategy) cleanCycleAndResetToIdle(cycleID uint64) {
	defer func() {
		if r := recover(); r != nil {
			utils.Logger.Error("cleanCycleAndResetToIdle panic 恢复", zap.Any("recover", r))
		}
	}()

	utils.Logger.Info("开始周期清理：撤销全部挂单", zap.Uint64("cycle_id", cycleID))
	if err := s.exchange.CancelAllOrders(); err != nil {
		utils.Logger.Warn("周期清理：撤单失败，重试一次", zap.Error(err))
		time.Sleep(500 * time.Millisecond)
		if err2 := s.exchange.CancelAllOrders(); err2 != nil {
			utils.Logger.Error("周期清理：重试撤单亦失败", zap.Error(err2))
		} else {
			utils.Logger.Info("周期清理：重试撤单成功")
		}
	} else {
		utils.Logger.Info("周期清理：挂单撤销完成")
	}

	s.resetToIdle(cycleID)
}

func (s *MartingaleStrategy) resetToIdle(cycleID uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.currentState == StateIdle || s.cycleID != cycleID {
		return false
	}
	s.currentState = StateIdle
	s.currentTPOrderID = 0
	s.baseOrderID = 0
	s.gridPlaced = false
	s.gridFilledCount = 0
	s.lastTPQty = 0
	s.lastTPPrice = 0
	s.lastTPFill = time.Now()
	s.initialEntryPrice = 0.0
	utils.Logger.Info("FSM 已重置为 IDLE", zap.Uint64("cycle_id", cycleID))
	return true
}

func (s *MartingaleStrategy) waitPositionZeroThenReset(cycleID uint64) {
	defer func() {
		if r := recover(); r != nil {
			utils.Logger.Error("waitPositionZeroThenReset panic 恢复", zap.Any("recover", r), zap.Stack("stack"))
		}
	}()

	const maxAttempts = 15 // 2s 间隔，最长约 30s
	for attempt := 0; attempt < maxAttempts; attempt++ {
		pos, err := s.exchange.GetPosition()
		if err != nil {
			utils.Logger.Warn("等待持仓归零：查询持仓失败",
				zap.Int("attempt", attempt+1), zap.Error(err))
		} else {
			amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			if math.Abs(amt) == 0 {
				utils.Logger.Info("持仓已归零，重置 FSM 为 IDLE", zap.Int("attempt", attempt+1))
				go s.cleanCycleAndResetToIdle(cycleID)
				return
			} else {
				utils.Logger.Debug("等待持仓归零：仍有持仓（可能部分成交）",
					zap.Float64("amt", amt), zap.Int("attempt", attempt+1))
			}
		}

		select {
		case <-s.ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	// 超时后仍有持仓：按"止盈部分成交"处理，重新对齐 TP，保持 IN_POSITION
	utils.Logger.Warn("等待持仓归零超时，按部分成交处理：重新对齐 TP")
	s.mu.RLock()
	state := s.currentState
	s.mu.RUnlock()
	if state == StateInPosition {
		go s.safeUpdateTP()
	}
}

// ---------------------------------------------------------------------------
// 策略动作
// ---------------------------------------------------------------------------

func (s *MartingaleStrategy) enterLong(currentPrice float64) error {
	utils.Logger.Info("Entering Long Position...")

	// Calculate Base Quantity (Level 1: 6% asset allocation of account balance)
	baseNotional := s.calcMinNotional()

	// 挂单价格：currentPrice + 2*tickSize，略高于当前价以提高成交概率
	limitPrice := currentPrice + 2*s.tickSize
	limitPrice = utils.RoundToTickSize(limitPrice, s.tickSize)
	limitPrice = utils.ToFixed(limitPrice, s.pricePrecision)

	// ★ 数量严格向下取整（Floor truncation），防止余额不足
	baseQty := utils.FloorToTickSize(baseNotional/limitPrice, s.stepSize)
	baseQty = utils.FloorToDecimals(baseQty, s.quantityPrecision)
	if baseQty < s.minQty {
		baseQty = s.minQty
	}
	if baseQty*limitPrice < MinOrderValue {
		qtyNeeded := MinOrderValue / limitPrice
		baseQty = utils.FloorToTickSize(qtyNeeded, s.stepSize)
		baseQty = utils.FloorToDecimals(baseQty, s.quantityPrecision)
		if baseQty*limitPrice < MinOrderValue {
			baseQty = utils.FloorToDecimals(baseQty+s.stepSize, s.quantityPrecision)
		}
	}

	utils.Logger.Info("Calculated Base Qty (Maker Limit)",
		zap.Float64("price", currentPrice),
		zap.Float64("limit_price", limitPrice),
		zap.Float64("base_notional", baseNotional),
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
			s.initialEntryPrice = 0.0
			s.mu.Unlock()
		}
	}
}

// ---------------------------------------------------------------------------
// 网格订单放置与重试
// ---------------------------------------------------------------------------

func (s *MartingaleStrategy) safePlaceGridOrders(execPrice float64) {
	const maxRetries = 3
	s.placeGridOrdersWithRetry(execPrice, 0, maxRetries)
}

func (s *MartingaleStrategy) placeGridOrdersWithRetry(execPrice float64, attempt, maxRetries int) {
	defer func() {
		if r := recover(); r != nil {
			utils.Logger.Error("placeGridOrders panic 恢复",
				zap.Any("recover", r),
				zap.Int("attempt", attempt+1),
				zap.Stack("stack"))
			if attempt+1 < maxRetries {
				go func() {
					time.Sleep(5 * time.Second)
					s.mu.RLock()
					state := s.currentState
					gridPlaced := s.gridPlaced
					s.mu.RUnlock()
					if state == StateInPosition && !gridPlaced {
						s.placeGridOrdersWithRetry(execPrice, attempt+1, maxRetries)
					}
				}()
			} else {
				utils.Logger.Error("placeGridOrders 已达最大重试次数，放弃",
					zap.Int("max_retries", maxRetries))
			}
		}
	}()
	s.placeGridOrders(execPrice)
}

func (s *MartingaleStrategy) placeOrderWithRetry(side futures.SideType, orderType futures.OrderType, qty, price float64, level int) bool {
	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		_, err := s.exchange.PlaceOrder(side, orderType, qty, price, false)
		if err == nil {
			return true
		}
		if attempt < maxRetries-1 {
			backoff := time.Duration(200*(1<<attempt)) * time.Millisecond
			utils.Logger.Warn("网格订单重试",
				zap.Int("level", level),
				zap.Int("attempt", attempt+1),
				zap.Error(err))
			time.Sleep(backoff)
		}
	}
	return false
}

func (s *MartingaleStrategy) placeGridOrders(execPrice float64) {
	utils.Logger.Info("placeGridOrders started", zap.Float64("execPrice", execPrice))

	// 1. 检查网格是否已放置
	s.mu.RLock()
	if s.gridPlaced {
		s.mu.RUnlock()
		utils.Logger.Warn("placeGridOrders skipped: grid already placed")
		return
	}
	s.mu.RUnlock()

	// 2. 检查现有挂单，验证完整性
	existingOrders, err := s.exchange.GetOpenOrders()
	if err == nil && len(existingOrders) > 0 {
		var existingBuyIDs []int64
		for _, o := range existingOrders {
			if o.Side == futures.SideTypeBuy {
				existingBuyIDs = append(existingBuyIDs, o.OrderID)
			}
		}
		if len(existingBuyIDs) == len(safetyOrderAllocations) {
			utils.Logger.Info("placeGridOrders: exact safety orders already present on exchange",
				zap.Int("count", len(existingBuyIDs)))
			s.mu.Lock()
			s.gridPlaced = true
			s.currentState = StateInPosition
			s.mu.Unlock()
			s.safeUpdateTP()
			return
		}
		if len(existingBuyIDs) > 0 {
			utils.Logger.Warn("placeGridOrders: cleaning up incomplete buy orders before fresh placement",
				zap.Int("existing_count", len(existingBuyIDs)))
			for _, id := range existingBuyIDs {
				_ = s.exchange.CancelOrder(id)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// 3. 加排他锁防并发
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
	if execPrice > 0 {
		entryPrice = execPrice
		utils.Logger.Info("Using execution price from order event", zap.Float64("entryPrice", entryPrice))
	} else {
		pos, err := s.exchange.GetPosition()
		if err != nil {
			utils.Logger.Error("Failed to get position for grid orders", zap.Error(err))
			return
		}
		entryPrice, _ = strconv.ParseFloat(pos.EntryPrice, 64)
		utils.Logger.Info("Using entry price from position API", zap.Float64("entryPrice", entryPrice))
	}

	if entryPrice <= 0 {
		utils.Logger.Error("Invalid entry price, cannot place grid orders", zap.Float64("entryPrice", entryPrice))
		return
	}

	balance, err := s.exchange.GetBalance()
	if err != nil {
		utils.Logger.Error("Failed to get balance for grid orders, using fallback", zap.Error(err))
		balance = MinOrderValue / 0.06
	}

	utils.Logger.Info("Placing Grid Orders",
		zap.Float64("Entry", entryPrice),
		zap.Float64("Balance", balance),
	)

	// Fixed percentage-based grid distances, relative to previous level
	// Level 1-9: 1.0%, 1.0%, 1.0%, 1.1%, 2.1%, 2.2%, 4.5%, 4.8%, 7.7%
	gridPcts := []float64{1.0, 1.0, 1.0, 1.1, 2.1, 2.2, 4.5, 4.8, 7.7}
	currentPriceLevel := entryPrice
	successCount := 0

	for i := 0; i < len(gridPcts); i++ {
		stepPct := gridPcts[i]
		price := currentPriceLevel * (1 - stepPct/100)
		currentPriceLevel = price

		price = utils.RoundToTickSize(price, s.tickSize)
		price = utils.ToFixed(price, s.pricePrecision)

		allocRatio := safetyOrderAllocations[i]
		orderNotional := balance * allocRatio

		if orderNotional < MinOrderValue {
			orderNotional = MinOrderValue
		}

		// ★ 严格向下取整
		qty := utils.FloorToTickSize(orderNotional/price, s.stepSize)
		qty = utils.FloorToDecimals(qty, s.quantityPrecision)
		if qty < s.minQty {
			qty = s.minQty
		}
		if qty*price < MinOrderValue {
			qtyNeeded := MinOrderValue / price
			qty = utils.FloorToTickSize(qtyNeeded, s.stepSize)
			qty = utils.FloorToDecimals(qty, s.quantityPrecision)
			if qty*price < MinOrderValue {
				qty = utils.FloorToDecimals(qty+s.stepSize, s.quantityPrecision)
			}
		}

		utils.Logger.Info("Placing Safety Order",
			zap.Int("index", i+1),
			zap.Float64("price", price),
			zap.Float64("notional", orderNotional),
			zap.Float64("alloc_pct", allocRatio*100),
			zap.Float64("qty", qty),
			zap.Float64("dist_pct", stepPct),
		)

		if s.placeOrderWithRetry(futures.SideTypeBuy, futures.OrderTypeLimit, qty, price, i+1) {
			successCount++
		}

		time.Sleep(200 * time.Millisecond)
	}

	s.mu.Lock()
	if successCount == len(gridPcts) {
		s.gridPlaced = true
		s.currentState = StateInPosition
		s.gridFilledCount = 0
		utils.Logger.Info("Grid orders placed successfully, gridPlaced=true", zap.Int("success_count", successCount))
	} else {
		s.gridPlaced = false
		utils.Logger.Warn("Grid orders placement incomplete, allowing retry",
			zap.Int("success_count", successCount), zap.Int("expected", len(gridPcts)))
	}
	s.mu.Unlock()

	s.safeUpdateTP()
}

// ---------------------------------------------------------------------------
// 止盈 (TP) 逻辑与对账
// ---------------------------------------------------------------------------

func (s *MartingaleStrategy) safeUpdateTP() {
	defer func() {
		if r := recover(); r != nil {
			utils.Logger.Error("updateTP panic 恢复", zap.Any("recover", r), zap.Stack("stack"))
			go func() {
				time.Sleep(5 * time.Second)
				s.mu.RLock()
				state := s.currentState
				s.mu.RUnlock()
				if state == StateInPosition {
					s.safeUpdateTP()
				}
			}()
		}
	}()

	if !s.tpMu.TryLock() {
		s.tpDirty.Store(true)
		s.mu.Lock()
		s.tpSkipCount++
		skipCount := s.tpSkipCount
		s.mu.Unlock()
		utils.Logger.Warn("updateTP 跳过：已在执行中，标记 dirty",
			zap.Int64("skip_count", skipCount))
		return
	}
	defer s.tpMu.Unlock()

	s.tpDirty.Store(false)
	s.updateTP()

	// 最多重跑 3 次脏标志
	const maxTPDirtyRetries = 3
	for i := 0; i < maxTPDirtyRetries && s.tpDirty.Load(); i++ {
		s.tpDirty.Store(false)
		utils.Logger.Info("检测到 dirty 标志，重跑 updateTP",
			zap.Int("retry", i+1),
			zap.Int("max_retries", maxTPDirtyRetries))
		s.updateTP()
	}
}

func (s *MartingaleStrategy) findLiveTP() (int64, float64, float64, error) {
	orders, err := s.exchange.GetOpenOrders()
	if err != nil {
		return 0, 0, 0, err
	}
	var tpOrders []*futures.Order
	for _, o := range orders {
		if o.Side == futures.SideTypeSell && o.Type == futures.OrderTypeLimit {
			tpOrders = append(tpOrders, o)
		}
	}
	if len(tpOrders) == 0 {
		return 0, 0, 0, nil
	}
	if len(tpOrders) > 1 {
		utils.Logger.Warn("发现多个 TP 订单（异常状态），保留第一个并清理其余",
			zap.Int("count", len(tpOrders)),
			zap.Int64("keep_id", tpOrders[0].OrderID))
		for i := 1; i < len(tpOrders); i++ {
			extraID := tpOrders[i].OrderID
			go func(id int64) {
				defer func() {
					if r := recover(); r != nil {
						utils.Logger.Error("清理多余 TP goroutine panic", zap.Any("recover", r))
					}
				}()
				_ = s.exchange.CancelOrder(id)
			}(extraID)
		}
	}
	qty, _ := strconv.ParseFloat(tpOrders[0].OrigQuantity, 64)
	price, _ := strconv.ParseFloat(tpOrders[0].Price, 64)
	return tpOrders[0].OrderID, qty, price, nil
}

func (s *MartingaleStrategy) updateTP() {
	utils.Logger.Info("updateTP started")

	// 1. 获取更新后的持仓
	pos, err := s.exchange.GetPosition()
	if err != nil {
		utils.Logger.Error("Failed to get position for TP update", zap.Error(err))
		return
	}

	avgPrice, _ := strconv.ParseFloat(pos.EntryPrice, 64)
	amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)

	// 如果持仓已清零，清除 TP
	if math.Abs(amt) == 0 {
		s.mu.Lock()
		s.currentTPOrderID = 0
		s.lastTPQty = 0
		s.lastTPPrice = 0
		s.mu.Unlock()
		utils.Logger.Info("持仓已清零，清除 TP 状态")
		return
	}

	s.mu.RLock()
	isIdle := s.currentState == StateIdle
	oldTPID := s.currentTPOrderID
	prevQty := s.lastTPQty
	s.mu.RUnlock()

	// 安全检查：如果状态为 IDLE，不更新 TP
	if isIdle {
		utils.Logger.Info("updateTP 跳过：状态为 IDLE")
		return
	}

	// 入口对账：若本地无 TP 记录，检查交易所端是否已有遗留 TP
	if oldTPID == 0 {
		liveID, liveQty, livePrice, reconcileErr := s.findLiveTP()
		if reconcileErr != nil {
			utils.Logger.Warn("入口对账：查询挂单失败", zap.Error(reconcileErr))
		} else if liveID != 0 {
			s.mu.Lock()
			s.currentTPOrderID = liveID
			s.lastTPQty = liveQty
			s.lastTPPrice = livePrice
			s.mu.Unlock()
			oldTPID = liveID
			prevQty = liveQty
			utils.Logger.Info("入口对账：认领交易所端已存在的 TP",
				zap.Int64("tp_id", liveID),
				zap.Float64("qty", liveQty),
				zap.Float64("price", livePrice))
		}
	}

	// ★ TP 数量严格向下取整
	newQty := utils.FloorToDecimals(math.Abs(amt), s.quantityPrecision)
	if newQty < s.minQty {
		newQty = s.minQty
	}

	// ★ 仓位变化检测：若仓位未变且已有 TP 订单，跳过更新
	if newQty == prevQty && oldTPID != 0 {
		utils.Logger.Debug("updateTP 跳过：仓位未变化",
			zap.Float64("qty", newQty),
			zap.Float64("prev_qty", prevQty),
			zap.Int64("tp_id", oldTPID))
		return
	}

	utils.Logger.Info("仓位变化，更新 TP",
		zap.Float64("prev_qty", prevQty),
		zap.Float64("new_qty", newQty),
		zap.Int64("old_tp_id", oldTPID))

	// TP = average entry price + 0.80%
	tpPrice := avgPrice * 1.008
	tpPrice = utils.RoundToTickSize(tpPrice, s.tickSize)
	tpPrice = utils.ToFixed(tpPrice, s.pricePrecision)

	// ★ 防追价校验：若 TP 价格不高于市价，跳过本次更新（防止限价卖单立即按市价成交）
	marketPrice, priceErr := s.exchange.GetLatestPrice()
	if priceErr != nil {
		utils.Logger.Warn("获取市价失败，跳过本次 TP 更新，等待下次重试", zap.Error(priceErr))
		return
	}
	if tpPrice <= marketPrice {
		utils.Logger.Warn("TP 价格已不高于市价，跳过 TP 更新（避免限价卖单立即成交）",
			zap.Float64("tp_price", tpPrice),
			zap.Float64("market_price", marketPrice),
			zap.Float64("entry_price", avgPrice))
		return
	}

	// ★ 优先使用 ModifyOrder 原子替换（避免取消+重建的空窗期）
	if oldTPID != 0 {
		resp, modErr := s.exchange.ModifyOrder(oldTPID, futures.SideTypeSell, newQty, tpPrice)
		if modErr == nil {
			s.mu.Lock()
			if s.currentState == StateIdle {
				s.mu.Unlock()
				utils.Logger.Info("Modify 成功但周期已结束，取消新 TP", zap.Int64("id", resp.OrderID))
				go func() {
					defer func() {
						if r := recover(); r != nil {
							utils.Logger.Error("取消 TP goroutine panic", zap.Any("recover", r))
						}
					}()
					_ = s.exchange.CancelOrder(resp.OrderID)
				}()
				return
			}
			if resp.OrderID != 0 {
				s.currentTPOrderID = resp.OrderID
			}
			s.lastTPQty = newQty
			s.lastTPPrice = tpPrice
			s.mu.Unlock()
			utils.Logger.Info("TP 已通过 ModifyOrder 更新",
				zap.Int64("tp_id", resp.OrderID),
				zap.Float64("qty", newQty),
				zap.Float64("price", tpPrice))
			return
		}

		// modify 失败：对账真实状态
		utils.Logger.Warn("Modify TP 失败，对账交易所端真实状态",
			zap.Int64("old_tp_id", oldTPID),
			zap.Error(modErr))

		liveID, liveQty, livePrice, reconcileErr := s.findLiveTP()
		if reconcileErr != nil {
			utils.Logger.Warn("对账查询失败，保守跳过本次更新，等下次重试", zap.Error(reconcileErr))
			return
		}

		if liveID != 0 && liveID != oldTPID {
			s.mu.Lock()
			if s.currentState == StateIdle {
				s.mu.Unlock()
				go func() {
					defer func() {
						if r := recover(); r != nil {
							utils.Logger.Error("取消 TP goroutine panic", zap.Any("recover", r))
						}
					}()
					_ = s.exchange.CancelOrder(liveID)
				}()
				return
			}
			s.currentTPOrderID = liveID
			s.lastTPQty = liveQty
			s.lastTPPrice = livePrice
			s.mu.Unlock()
			utils.Logger.Info("Modify 网络失败但交易所端已成功，同步本地状态，跳过 create",
				zap.Int64("old_tp_id", oldTPID),
				zap.Int64("new_tp_id", liveID),
				zap.Float64("qty", liveQty),
				zap.Float64("price", livePrice))
			return
		}

		if liveID == oldTPID && oldTPID != 0 {
			utils.Logger.Info("旧 TP 仍在，取消后重建", zap.Int64("id", oldTPID))
			_ = s.exchange.CancelOrder(oldTPID)
		}
	}

	// 放置新 TP 订单（reduceOnly=true）
	utils.Logger.Info("Placing New 100% Full Position TP Order",
		zap.Float64("EntryPrice", avgPrice),
		zap.Float64("TPPrice", tpPrice),
		zap.Float64("TotalPositionAmt", math.Abs(amt)),
		zap.Float64("TPQty", newQty),
	)

	resp, err := s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeLimit, newQty, tpPrice, true)
	if err != nil {
		utils.Logger.Warn("Failed to place TP order, retrying once", zap.Error(err))
		time.Sleep(500 * time.Millisecond)
		resp, err = s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeLimit, newQty, tpPrice, true)
		if err != nil {
			utils.Logger.Error("Failed to place TP order after retry", zap.Error(err))
			return
		}
	}

	s.mu.Lock()
	if s.currentState == StateIdle {
		s.mu.Unlock()
		utils.Logger.Info("Cycle finished during TP update, cancelling new TP", zap.Int64("id", resp.OrderID))
		go func() {
			defer func() {
				if r := recover(); r != nil {
					utils.Logger.Error("取消 TP goroutine panic", zap.Any("recover", r))
				}
			}()
			_ = s.exchange.CancelOrder(resp.OrderID)
		}()
		return
	}
	s.currentTPOrderID = resp.OrderID
	s.lastTPQty = newQty
	s.lastTPPrice = tpPrice
	s.mu.Unlock()

	utils.Logger.Info("TP 已通过 PlaceOrder 更新",
		zap.Int64("tp_id", resp.OrderID),
		zap.Float64("qty", newQty),
		zap.Float64("price", tpPrice))
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
