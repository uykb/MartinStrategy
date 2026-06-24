package strategy

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

// ── Dashboard DTO types ──────────────────────────────────────────────

// DashboardState is the JSON snapshot pushed to the web dashboard via SSE.
type DashboardState struct {
	Running    bool          `json:"running"`
	Paused     bool          `json:"paused"`
	State      string        `json:"state"`
	StateLabel string        `json:"stateLabel"`
	Symbol     string        `json:"symbol"`

	Balance float64 `json:"balance"`
	Equity  float64 `json:"equity"`

	Position   *PositionInfo `json:"position"`
	OpenOrders []OrderInfo   `json:"openOrders"`
	TPOrder    *OrderInfo    `json:"tpOrder"`

	Grid *GridInfo `json:"grid"`

	Fills  []FillInfo  `json:"fills"`
	Alerts []AlertInfo `json:"alerts"`

	UpdatedAt int64 `json:"updatedAt"`
}

// PositionInfo describes the current position for the dashboard.
type PositionInfo struct {
	HasPosition   bool    `json:"hasPosition"`
	Side          string  `json:"side"`
	Size          float64 `json:"size"`
	EntryPrice    float64 `json:"entryPrice"`
	MarkPrice     float64 `json:"markPrice"`
	Leverage      int     `json:"leverage"`
	UnrealizedPnl float64 `json:"unrealizedPnl"`
	UnrealizedPct float64 `json:"unrealizedPct"`
}

// OrderInfo describes a single open order.
type OrderInfo struct {
	OrderID    int64   `json:"orderId"`
	Side       string  `json:"side"`
	Type       string  `json:"type"`
	Price      float64 `json:"price"`
	Quantity   float64 `json:"quantity"`
	Status     string  `json:"status"`
	ReduceOnly bool    `json:"reduceOnly"`
}

// GridInfo summarises the grid state.
type GridInfo struct {
	Placed       bool `json:"placed"`
	MaxLevels    int  `json:"maxLevels"`
	FilledLevels int  `json:"filledLevels"` // 已成交的安全单数量
	PlacedLevels int  `json:"placedLevels"` // 已放置的安全单总数（已成交 + 挂单中）
}

// FillInfo records a recent order fill.
type FillInfo struct {
	Time     string  `json:"time"`
	Side     string  `json:"side"`
	Price    float64 `json:"price"`
	Quantity float64 `json:"quantity"`
	Type     string  `json:"type"`
}

// AlertInfo records a recent log/alert event.
type AlertInfo struct {
	Time    string `json:"time"`
	Message string `json:"message"`
}

// ── Constants ────────────────────────────────────────────────────────

const (
	maxFills  = 50
	maxAlerts = 50
)

// ── Snapshot ────────────────────────────────────────────────────────

// Snapshot returns a thread-safe read-only copy of the strategy state for the dashboard.
func (s *MartingaleStrategy) Snapshot() *DashboardState {
	s.mu.RLock()
	defer s.mu.RUnlock()

	st := &DashboardState{
		Running:    !s.paused,
		Paused:     s.paused,
		State:      string(s.currentState),
		StateLabel: stateLabel(s.currentState),
		Symbol:     s.exchange.GetSymbol(),
		Balance:    s.cachedBalance,
		Grid: &GridInfo{
			Placed:       s.gridPlaced,
			MaxLevels:    s.cfg.MaxSafetyOrders,
			FilledLevels: s.gridFilledCount,
			PlacedLevels: s.gridFilledCount + s.countPendingSafetyOrders(),
		},
		Fills:     s.fills,
		Alerts:    s.alerts,
		UpdatedAt: time.Now().Unix(),
	}

	// Position
if s.cachedPosition != nil {
		amt, _ := strconv.ParseFloat(s.cachedPosition.PositionAmt, 64)
		entry, _ := strconv.ParseFloat(s.cachedPosition.EntryPrice, 64)
		upnl, _ := strconv.ParseFloat(s.cachedPosition.UnrealizedProfit, 64)
		lev, _ := strconv.Atoi(s.cachedPosition.Leverage)

		if math.Abs(amt) > 0 {
			st.Position = &PositionInfo{
				HasPosition:   true,
				Side:         "LONG",
				Size:         amt,
				EntryPrice:   entry,
				MarkPrice:    0, // AccountPosition has no MarkPrice; use 0 as placeholder
				Leverage:     lev,
				UnrealizedPnl: upnl,
			}
			if entry > 0 {
				st.Position.UnrealizedPct = (0 - entry) / entry * 100 // markPrice unavailable
			}
			st.Equity = st.Balance + upnl
		}
	}
	if st.Position == nil {
		st.Position = &PositionInfo{HasPosition: false}
		st.Equity = st.Balance
	}

	// Open orders (exclude TP)
	for _, o := range s.cachedOrders {
		if o.OrderID == s.currentTPOrderID {
			continue
		}
		price, _ := strconv.ParseFloat(o.Price, 64)
		qty, _ := strconv.ParseFloat(o.OrigQuantity, 64)
		st.OpenOrders = append(st.OpenOrders, OrderInfo{
			OrderID:    o.OrderID,
			Side:       string(o.Side),
			Type:       string(o.Type),
			Price:      price,
			Quantity:   qty,
			Status:     string(o.Status),
			ReduceOnly: o.ReduceOnly,
		})
	}

	// TP order
	if s.currentTPOrderID != 0 {
		for _, o := range s.cachedOrders {
			if o.OrderID == s.currentTPOrderID {
				price, _ := strconv.ParseFloat(o.Price, 64)
				qty, _ := strconv.ParseFloat(o.OrigQuantity, 64)
				st.TPOrder = &OrderInfo{
					OrderID:    o.OrderID,
					Side:       string(o.Side),
					Type:       string(o.Type),
					Price:      price,
					Quantity:   qty,
					Status:     string(o.Status),
					ReduceOnly: o.ReduceOnly,
				}
				break
			}
		}
	}

	return st
}

// ── Control methods ─────────────────────────────────────────────────

// Pause stops the strategy from opening new positions. Existing orders and positions are preserved.
func (s *MartingaleStrategy) Pause() error {
	s.mu.Lock()
	s.paused = true
	s.mu.Unlock()
	s.addAlert("策略已暂停 — 不再开新仓，持仓与挂单保留")
	return nil
}

// Resume re-enables the strategy after a Pause.
func (s *MartingaleStrategy) Resume() error {
	s.mu.Lock()
	s.paused = false
	s.mu.Unlock()
	s.addAlert("策略已恢复")
	return nil
}

// CloseAll cancels all orders and market-closes any open position, then resets to IDLE.
func (s *MartingaleStrategy) CloseAll() error {
	s.addAlert("手动全部平仓 — 撤单并市价平仓")
	go func() {
		if err := s.exchange.CancelAllOrders(); err != nil {
			s.addAlert(fmt.Sprintf("撤单失败: %v", err))
		}
		pos, err := s.exchange.GetPosition()
		if err == nil {
			amt, _ := strconv.ParseFloat(pos.PositionAmt, 64)
			if math.Abs(amt) > 0 {
				if _, err := s.exchange.PlaceOrder(futures.SideTypeSell, futures.OrderTypeMarket, math.Abs(amt), 0, true); err != nil {
					s.addAlert(fmt.Sprintf("市价平仓失败: %v", err))
				} else {
					s.addAlert("市价平仓单已提交")
				}
			}
		}
		s.mu.Lock()
		s.currentState = StateIdle
		s.gridPlaced = false
		s.currentTPOrderID = 0
		s.gridFilledCount = 0
		s.baseOrderID = 0
		s.mu.Unlock()
	}()
	return nil
}

// IsPaused returns whether the strategy is currently paused.
func (s *MartingaleStrategy) IsPaused() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.paused
}

// RefreshTP re-calculates and updates the take-profit order based on current position and ATR.
func (s *MartingaleStrategy) RefreshTP() error {
	s.addAlert("手动刷新止盈")
	go s.updateTP()
	return nil
}

// countPendingSafetyOrders counts buy orders currently open on the exchange (safety orders not yet filled).
func (s *MartingaleStrategy) countPendingSafetyOrders() int {
	if !s.gridPlaced {
		return 0
	}
	count := 0
	for _, o := range s.cachedOrders {
		if o.Side == futures.SideTypeBuy {
			count++
		}
	}
	return count
}

// ── Fills & Alerts ring buffer ──────────────────────────────────────

func (s *MartingaleStrategy) addFill(side, fillType string, price, qty float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fills = append(s.fills, FillInfo{
		Time:     time.Now().Format("15:04:05"),
		Side:     side,
		Price:    price,
		Quantity: qty,
		Type:     fillType,
	})
	if len(s.fills) > maxFills {
		s.fills = s.fills[len(s.fills)-maxFills:]
	}
}

func (s *MartingaleStrategy) addAlert(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alerts = append(s.alerts, AlertInfo{
		Time:    time.Now().Format("15:04:05"),
		Message: msg,
	})
	if len(s.alerts) > maxAlerts {
		s.alerts = s.alerts[len(s.alerts)-maxAlerts:]
	}
}

// ── Cache refresh ───────────────────────────────────────────────────

// refreshCacheLoop periodically fetches balance, position, and open orders from the exchange
// so that Snapshot() can return data without making API calls.
func (s *MartingaleStrategy) refreshCacheLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.refreshCache()
	}
}

func (s *MartingaleStrategy) refreshCache() {
	balance, _ := s.exchange.GetBalance()
	pos, _ := s.exchange.GetPosition()
	orders, _ := s.exchange.GetOpenOrders()

	s.mu.Lock()
	s.cachedBalance = balance
	s.cachedPosition = pos
	s.cachedOrders = orders
	s.mu.Unlock()
}

// ── Helpers ──────────────────────────────────────────────────────────

func stateLabel(st State) string {
	switch st {
	case StateIdle:
		return "空闲"
	case StateWaitingEntry:
		return "等待入场"
	case StatePlacingGrid:
		return "放置网格"
	case StateInPosition:
		return "持仓中"
	case StateClosing:
		return "平仓中"
	default:
		return string(st)
	}
}