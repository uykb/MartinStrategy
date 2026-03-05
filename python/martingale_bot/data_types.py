from enum import Enum
from dataclasses import dataclass, field
from typing import List, Optional
from decimal import Decimal
import time

class EventType(Enum):
    TICK = "TICK"             # 市场行情推送
    ORDER_UPDATE = "ORDER"    # 订单状态更新
    POSITION_UPDATE = "POS"   # 持仓更新
    LOG = "LOG"               # 日志事件

class OrderStatus(Enum):
    NEW = "NEW"
    PARTIALLY_FILLED = "PARTIALLY_FILLED"
    FILLED = "FILLED"
    CANCELED = "CANCELED"
    REJECTED = "REJECTED"

class OrderSide(Enum):
    BUY = "BUY"
    SELL = "SELL"

class OrderType(Enum):
    MARKET = "MARKET"
    LIMIT = "LIMIT"

class VolumeMode(Enum):
    GEOMETRIC = "GEOMETRIC" # 等比数列 (1, 2, 4, 8...)
    FIBONACCI = "FIBONACCI" # 斐波那契 (1, 1, 2, 3, 5...)
    LINEAR = "LINEAR"       # 等差 (1, 2, 3, 4...)

@dataclass
class TickData:
    symbol: str
    price: float
    timestamp: float = field(default_factory=time.time)

@dataclass
class OrderData:
    symbol: str
    order_id: str
    side: OrderSide
    type: OrderType
    price: float
    quantity: float
    filled_quantity: float
    status: OrderStatus
    timestamp: float = field(default_factory=time.time)
    tag: str = ""  # e.g., "BASE", "SAFETY_1", "TP"

@dataclass
class PositionData:
    symbol: str
    quantity: float
    average_price: float
    current_price: float = 0.0
    unrealized_pnl: float = 0.0

@dataclass
class StrategyConfig:
    symbol: str = "HYPEUSDT"
    base_order_size: float = 100.0       # 首单金额 (USDT)
    safety_order_size: float = 100.0     # 首个加仓单金额 (USDT)
    max_safety_orders: int = 9           # 最大加仓次数 (default 9 to match image)
    
    # 交易量控制
    volume_mode: VolumeMode = VolumeMode.GEOMETRIC
    safety_order_step_scale: float = 2.0 # 仅在 GEOMETRIC 模式下生效
    
    # 网格间距控制
    use_atr_for_grid: bool = True        # 强制使用 ATR
    atr_period: int = 14                 # ATR 计算周期
    atr_interval: str = "15m"            # ATR K线周期 (15min)
    fixed_atr_value: float = 0.0         # 测试用
    
    # 网格间距倍数列表
    grid_step_multipliers: Optional[List[float]] = None 
    
    safety_order_price_deviation: float = 0.01 
    safety_order_step_deviation: float = 1.0   
    
    # 止盈
    # 动态止盈：Profit = ATR * multiplier
    target_profit_atr_multiplier: float = 1.0 # 默认为 1倍 ATR
    target_profit: float = 0.015         # 固定百分比备用
    
    # 风控
    stop_loss_percentage: float = 0.10   # 10% 止损
