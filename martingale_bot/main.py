import asyncio
from martingale_bot.event_engine import EventEngine, Event, EventType
from martingale_bot.strategy import MartingaleStrategy
from martingale_bot.data_types import StrategyConfig, TickData, OrderData, OrderStatus, OrderSide, OrderType, VolumeMode
from martingale_bot.mock_exchange import MockExchange

async def main():
    # 1. 初始化基础设施
    event_engine = EventEngine()
    event_engine.start()
    
    mock_exchange = MockExchange(event_engine)
    
    # 2. 配置策略 (基于图片逻辑)
    config = StrategyConfig(
        symbol="BTC/USDT",
        base_order_size=100.0,
        safety_order_size=100.0,
        max_safety_orders=9,
        target_profit=0.015,
        
        # 新增配置
        volume_mode=VolumeMode.FIBONACCI, # 1, 1, 2, 3, 5, 8...
        use_atr_for_grid=True,
        fixed_atr_value=0.322, # 图片中的 ATR 值
        grid_step_multipliers=[1.0, 1.0, 1.0, 1.0, 2.0, 2.0, 4.0, 4.0, 6.0] # 自定义网格间距倍数
    )
    
    # 3. 初始化策略
    strategy = MartingaleStrategy(event_engine, config, mock_exchange)
    
    # 4. 启动策略
    print(">>> Starting Bot...")
    await strategy.start()
    
    # 5. 模拟一些事件来测试流程 (Mock Loop)
    print("\n>>> Simulating Market Events...")
    
    # 模拟：策略启动后，发现无持仓，会自动触发 prepare_new_cycle -> update_atr -> place_base_order
    # 我们这里模拟首单成交回报
    await asyncio.sleep(1)
    
    # 模拟交易所推送订单成交事件 (使用图片中的价格: 32.849)
    base_order_filled = OrderData(
        symbol="BTC/USDT", order_id="1", side=OrderSide.BUY, type=OrderType.MARKET,
        price=32.849, quantity=3.04, filled_quantity=3.04, status=OrderStatus.FILLED,
        tag="BASE"
    )
    
    # 必须先初始化持仓，因为策略逻辑依赖它
    from martingale_bot.data_types import PositionData
    strategy.current_position = PositionData(
        symbol="BTC/USDT", quantity=3.04, average_price=32.849
    )
    
    print(f"\n>>> Simulating BASE Order Filled: {base_order_filled.filled_quantity} @ {base_order_filled.price}")
    event_engine.put(Event(EventType.ORDER_UPDATE, base_order_filled))
    
    # 等待策略响应（策略应该会挂出加仓单和止盈单）
    await asyncio.sleep(2)
    
    # 模拟：价格下跌，触发第一个加仓单成交 (价格 32.527)
    print("\n>>> Simulating Price Drop & Safety Order Fill...")
    safety_order_filled = OrderData(
        symbol="BTC/USDT", order_id="2", side=OrderSide.BUY, type=OrderType.LIMIT,
        price=32.527, quantity=3.07, filled_quantity=3.07, status=OrderStatus.FILLED,
        tag="SAFETY_1"
    )
    
    # 更新持仓: 简单累加模拟
    strategy.current_position.quantity += 3.07
    # new_avg = (old_cost + new_cost) / total_qty
    new_cost = (3.04 * 32.849) + (3.07 * 32.527)
    strategy.current_position.average_price = new_cost / strategy.current_position.quantity
    
    event_engine.put(Event(EventType.ORDER_UPDATE, safety_order_filled))

    # 保持运行以便观察日志
    await asyncio.sleep(5)

    event_engine.stop()

if __name__ == "__main__":
    asyncio.run(main())
