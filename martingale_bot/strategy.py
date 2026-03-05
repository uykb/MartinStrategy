import asyncio
from typing import Dict, List, Optional
from .data_types import *
from .event_engine import EventEngine, Event
from .indicators import TechnicalAnalysis

class MartingaleStrategy:
    """
    基于状态机和全预挂单模式的马丁策略
    """
    def __init__(self, engine: EventEngine, config: StrategyConfig, exchange_api):
        self.engine = engine
        self.config = config
        self.exchange = exchange_api # BinanceClient (or Mock)
        
        # 内部状态
        self.is_active = False
        self.current_position: Optional[PositionData] = None
        self.active_orders: Dict[str, OrderData] = {} # order_id -> OrderData
        
        # 动态指标
        self.current_atr = 0.0
        
        # 策略计数器
        self.safety_orders_count = 0
        
        # 注册回调
        self.engine.register(EventType.TICK, self.on_tick)
        self.engine.register(EventType.ORDER_UPDATE, self.on_order_update)
        self.engine.register(EventType.POSITION_UPDATE, self.on_position_update)

    async def start(self):
        """策略启动入口"""
        print(f"Strategy Started: {self.config.symbol}")
        self.is_active = True
        
        # 1. 初始化同步状态
        await self.sync_state()
        
        # 2. 执行核心循环检查
        await self.check_logic()

    async def sync_state(self):
        """
        同步交易所状态 (持仓、挂单)
        用于重启恢复或定期校准
        """
        print("Syncing state with exchange...")
        # 获取真实持仓
        pos = await self.exchange.get_position_risk(self.config.symbol)
        if pos:
            self.current_position = pos
            print(f"Synced Position: {pos.quantity} @ {pos.average_price}")
        else:
            print("No position found on exchange.")
            self.current_position = PositionData(self.config.symbol, 0, 0)

        # 获取挂单 (TODO: 如果是真实环境，需要在这里重建 active_orders)
        # self.active_orders = ... 
        pass

    async def check_logic(self):
        """
        核心状态机逻辑 - 决定下一步行动
        """
        if not self.is_active:
            return

        # 场景 A: 无持仓 -> 启动新一轮 (Place Base Order)
        if not self.current_position or self.current_position.quantity <= 0:
            print("No position detected. Preparing to start new cycle.")
            await self.prepare_new_cycle()
            return

        # 场景 B: 有持仓 -> 检查网格挂单和止盈单是否健康
        if self.current_position.quantity > 0:
            await self.audit_grid_orders()

    async def prepare_new_cycle(self):
        """
        新一轮开始前的准备工作：
        1. 获取最新 15m ATR
        2. 下首单
        """
        print(">>> Preparing New Cycle...")
        
        # 1. 获取 ATR
        await self.update_atr()
        
        # 2. 下首单
        await self.place_base_order()

    async def update_atr(self):
        """更新 ATR 指标"""
        print(f"Fetching {self.config.atr_interval} Klines for ATR...")
        klines = await self.exchange.get_klines(self.config.symbol, interval=self.config.atr_interval, limit=50)
        if klines:
            self.current_atr = TechnicalAnalysis.calculate_atr(klines, period=self.config.atr_period)
            print(f"Updated ATR ({self.config.atr_interval}): {self.current_atr}")
        else:
            print("Failed to fetch klines for ATR. Using fallback/last value.")

    async def place_base_order(self):
        """
        下单逻辑：市价开仓
        """
        print(f"Placing BASE order for {self.config.base_order_size} USDT")
        
        # 获取当前价格估算数量 (真实下单时如果是市价单，Binance U本位合约通常按 quantity 下单，所以需要先算好)
        current_price = await self.exchange.get_symbol_price(self.config.symbol)
        if current_price <= 0:
            print("Error: Could not get current price.")
            return

        qty = self.config.base_order_size / current_price
        # TODO: 需要处理精度问题 (stepSize)
        
        # 发送订单
        # order = await self.exchange.create_order(self.config.symbol, OrderSide.BUY, OrderType.MARKET, qty)
        # if order:
        #    print(f"Base Order Placed: {order.order_id}")
        
        # 模拟成交 (Mock)
        # 在真实环境中，这里不需要做任何事，等待 websocket 推送 ORDER_FILLED 事件
        # Mock 模式下，我们在 main.py 里手动触发
        pass

    async def place_grid_orders(self, base_price: float):
        """
        核心逻辑：批量部署加仓单 (Safety Orders) 和 止盈单 (TP Order)
        """
        print(f"Deploying Grid based on price: {base_price}")
        
        safety_orders = []
        prev_price = base_price
        
        # 使用当前最新的 ATR (在新一轮开始时已获取)
        # 如果获取失败，使用 Config 中的固定值或默认值
        grid_atr = self.current_atr if self.current_atr > 0 else self.config.fixed_atr_value
        
        if grid_atr <= 0:
            print("CRITICAL WARNING: ATR is 0. Grid layout may be incorrect!")
            # 可以在这里做一个 fallback，比如 price * 1%
            
        print(f"Using ATR for Grid: {grid_atr}")
        
        for i in range(1, self.config.max_safety_orders + 1):
            # 1. 计算交易量 (Volume)
            vol_multiplier = self._get_volume_multiplier(i)
            target_amount_usdt = self.config.safety_order_size * vol_multiplier
            
            # 2. 计算价格 (Price)
            price = self._get_grid_price(i, base_price, prev_price, grid_atr)
            
            # 计算币的数量
            qty = target_amount_usdt / price
            
            safety_orders.append({
                "price": price,
                "qty": qty,
                "tag": f"SAFETY_{i}"
            })
            
            # 更新 prev_price
            prev_price = price
            
        # 2. 批量下单
        print(f"Generated {len(safety_orders)} safety orders. Sending to exchange...")
        for order in safety_orders:
            print(f"  [Plan] {order['tag']}: {order['qty']:.4f} @ {order['price']:.4f} (Drop: {base_price - order['price']:.4f})")
            # 真实下单:
            # await self.exchange.create_order(...)
        
        # 3. 挂止盈单
        await self.update_tp_order()

    def _get_volume_multiplier(self, index: int) -> float:
        """计算第 index 个加仓单的交易量倍数"""
        if self.config.volume_mode == VolumeMode.FIBONACCI:
            if index <= 0: return 0
            if index == 1: return 1
            if index == 2: return 1
            a, b = 1, 1
            for _ in range(index - 2):
                a, b = b, a + b
            return b
        elif self.config.volume_mode == VolumeMode.LINEAR:
            return float(index)
        else:
            # GEOMETRIC
            return self.config.safety_order_step_scale ** (index - 1)

    def _get_grid_price(self, index: int, base_price: float, prev_price: float, atr: float) -> float:
        """计算第 index 个加仓单的价格"""
        step_drop = 0.0
        
        # 模式 A: ATR Grid (强制启用)
        if atr > 0:
            multiplier = 1.0
            if self.config.grid_step_multipliers:
                if index <= len(self.config.grid_step_multipliers):
                    multiplier = self.config.grid_step_multipliers[index-1]
                else:
                    multiplier = self.config.grid_step_multipliers[-1]
            else:
                multiplier = self.config.safety_order_step_deviation ** (index - 1)
            
            step_drop = atr * multiplier
            return prev_price - step_drop

        # Fallback
        else:
            step_percent = self.config.safety_order_price_deviation * (self.config.safety_order_step_deviation ** (index - 1))
            step_drop = prev_price * step_percent
            return prev_price - step_drop

    async def update_tp_order(self):
        """
        动态调整止盈单 (TP Order)
        逻辑：
        1. 获取当前持仓均价 (Average Price)
        2. 获取当前最新的 ATR (15min)
        3. 计算目标止盈价 = 均价 + (1.0 * ATR)
           (如果做空，则是 均价 - ATR)
        4. 撤销旧 TP (如果存在)
        5. 挂新 TP (数量 = 当前全部持仓量)
        """
        if not self.current_position or self.current_position.quantity <= 0:
            return

        avg_price = self.current_position.average_price
        
        # 1. 优先使用实时 ATR 计算止盈
        # 如果 ATR 为 0 (未获取到)，回退到 Config 中的 fixed_atr_value
        atr_used = self.current_atr if self.current_atr > 0 else self.config.fixed_atr_value
        
        tp_price = 0.0
        
        if atr_used > 0:
            # 止盈点位 = 均价 + 1.0 * ATR
            # (这里假设是多头策略，如果是空头策略需要减)
            tp_spread = atr_used * self.config.target_profit_atr_multiplier # multiplier 默认为 1.0
            tp_price = avg_price + tp_spread
            print(f"Calc TP using ATR: {avg_price:.4f} + {atr_used:.4f} (Multiplier {self.config.target_profit_atr_multiplier}) = {tp_price:.4f}")
        else:
            # Fallback (仅当 ATR 完全无法获取时)
            tp_price = avg_price * (1 + self.config.target_profit)
            print(f"WARNING: ATR is 0. Calc TP using fixed %: {tp_price:.4f}")

        # 2. 止盈数量 = 当前全部持仓
        tp_qty = self.current_position.quantity
        
        print(f"Updating TP Order: Price {tp_price:.2f}, Qty {tp_qty}")
        
        # 3. 执行撤单与挂单
        # 在真实环境中，建议使用 API 的 "Cancel Replace" (如果支持) 或者先 Cancel 再 Place
        # 这里模拟先撤后挂
        
        # await self.exchange.cancel_order(symbol=self.config.symbol, order_id="TP_ORDER_ID") # 需维护 TP ID
        # await self.exchange.create_order(
        #     symbol=self.config.symbol, 
        #     side=OrderSide.SELL, # 平多
        #     type=OrderType.LIMIT, 
        #     price=tp_price, 
        #     quantity=tp_qty,
        #     tag="TP"
        # )
        pass

    async def on_order_update(self, event: Event):
        """
        订单状态回调
        """
        order: OrderData = event.data
        print(f"Order Update: {order.tag} {order.status} {order.filled_quantity}@{order.price}")

        # 如果是首单成交 -> 部署网格
        if order.tag == "BASE" and order.status == OrderStatus.FILLED:
            await self.place_grid_orders(base_price=order.price)
        
        # 如果是加仓单成交 -> 更新止盈
        elif "SAFETY" in order.tag and order.status == OrderStatus.FILLED:
            print("Safety order filled! Recalculating TP...")
            await self.update_tp_order()
            
        # 如果是止盈单成交 -> 结束本轮，准备下一轮
        elif order.tag == "TP" and order.status == OrderStatus.FILLED:
            print("TP Filled! Cycle Completed.")
            await self.cancel_all_orders() # 撤销剩余加仓单
            
            # 重启下一轮
            await asyncio.sleep(1) # 稍微冷却
            await self.prepare_new_cycle()

    async def on_tick(self, event: Event):
        pass

    async def on_position_update(self, event: Event):
        self.current_position = event.data

    async def audit_grid_orders(self):
        pass

    async def cancel_all_orders(self):
        print("Cancelling all open orders...")
        await self.exchange.cancel_all_orders(self.config.symbol)
