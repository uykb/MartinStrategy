import asyncio
from typing import List, Dict, Optional
from .data_types import OrderData, OrderStatus, OrderSide, OrderType, PositionData

class MockExchange:
    """
    模拟交易所，用于测试策略逻辑
    """
    def __init__(self, engine):
        self.engine = engine
        self.orders = {}
        self.position = PositionData(symbol="BTC/USDT", quantity=0, average_price=0)

    async def create_order(self, symbol, side, type, quantity, price=None, tag="", time_in_force="GTC"):
        # 模拟下单
        print(f"[MOCK EXCHANGE] Created Order: {side} {quantity} @ {price} ({tag})")
        return "mock_order_id"

    async def cancel_order(self, symbol, order_id):
        print(f"[MOCK EXCHANGE] Cancelled Order: {order_id}")
        return True
    
    async def cancel_all_orders(self, symbol):
        print(f"[MOCK EXCHANGE] Cancelled All Orders for {symbol}")
        return True

    async def get_position_risk(self, symbol):
        # 模拟 API 行为: 如果数量为 0，可能返回空或者 0 持仓
        # 这里直接返回内部维护的对象
        return self.position

    async def get_symbol_price(self, symbol):
        return 32849.0 # Mock price
        
    async def get_klines(self, symbol, interval, limit):
        # 模拟 K 线数据 (用于 ATR 计算)
        # 格式: [time, open, high, low, close, vol...]
        # 造一些波动数据，让 ATR 有值
        import time
        now = int(time.time() * 1000)
        klines = []
        base = 32000.0
        for i in range(limit):
            # high-low = 322 (to match ATR=0.322 roughly? No, ATR is average)
            # let's make H-L = 0.322
            klines.append([
                now - (limit-i)*900000, 
                str(base), str(base+0.322), str(base), str(base+0.1), # O, H, L, C
                "100", 0, 0, 0, 0, 0, "0" # Ignore field added
            ])
        return klines
