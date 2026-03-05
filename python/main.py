import asyncio
import subprocess
import json
import os
import threading
import sys
import aiohttp
from martingale_bot.event_engine import EventEngine, Event, EventType
from martingale_bot.strategy import MartingaleStrategy
from martingale_bot.data_types import StrategyConfig, TickData, OrderData, OrderStatus, OrderSide, OrderType, VolumeMode

# --- Go Adapter ---

class GoExchangeAdapter:
    """
    Python -> Go (via HTTP)
    Go -> Python (via Stdout Pipe)
    """
    def __init__(self, engine: EventEngine, go_url="http://localhost:8080"):
        self.engine = engine
        self.go_url = go_url
        self.session = None

    async def init_session(self):
        if not self.session:
            self.session = aiohttp.ClientSession()

    async def create_order(self, symbol, side, type, quantity, price=None, tag="", time_in_force="GTC"):
        await self.init_session()
        payload = {
            "symbol": symbol,
            "side": side.value,
            "type": type.value,
            "quantity": float(quantity),
            "price": float(price) if price else 0.0
        }
        try:
            async with self.session.post(f"{self.go_url}/order", json=payload) as resp:
                if resp.status == 200:
                    data = await resp.json()
                    # Convert to OrderData
                    return OrderData(
                        symbol=symbol,
                        order_id=str(data.get("orderId")),
                        side=side,
                        type=type,
                        price=float(data.get("price", 0)),
                        quantity=float(data.get("origQty", 0)),
                        filled_quantity=float(data.get("executedQty", 0)),
                        status=OrderStatus(data.get("status")),
                        tag=tag
                    )
                else:
                    err = await resp.text()
                    print(f"Go Order Error: {err}")
                    return None
        except Exception as e:
            print(f"Go Order Exception: {e}")
            return None

    async def cancel_all_orders(self, symbol):
        await self.init_session()
        try:
            async with self.session.post(f"{self.go_url}/cancelAll?symbol={symbol}") as resp:
                return resp.status == 200
        except Exception as e:
            print(f"Go Cancel Error: {e}")
            return False

    async def get_position_risk(self, symbol):
        await self.init_session()
        # TODO: Implement Position parsing from Go response
        # Mock for now if Go side not fully ready for complex JSON
        pass

    async def get_klines(self, symbol, interval, limit):
        # Go doesn't implement Klines yet, use direct Binance REST in Python or add to Go
        # For simplicity, we can keep using Python for Klines (Analysis) 
        # as it's not high-frequency critical.
        # But wait, we want "Stability". Let's assume Python handles Analysis IO.
        pass

# --- Main Process ---

def run_go_process(engine):
    """
    Launch Go binary and read stdout
    """
    # Check if we are in Docker (binary at /usr/local/bin/go-bot) or local
    cmd = ["go", "run", "../go/main.go"] # Local dev
    if os.path.exists("/usr/local/bin/go-bot"):
        cmd = ["/usr/local/bin/go-bot"]
    
    print(f"Launching Go Process: {cmd}")
    process = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=sys.stderr, # Forward Go logs to stderr
        text=True,
        bufsize=1,
        env=os.environ.copy()
    )
    
    for line in process.stdout:
        try:
            data = json.loads(line)
            if data.get("type") == "TICK":
                # Push to Engine
                # print(f"Tick received: {data['price']}")
                # Construct TickData...
                pass
        except json.JSONDecodeError:
            pass # Ignore non-json logs
            
async def main():
    # ... setup ...
    print("Initializing Python Strategy Brain...")
    
    # 1. Start Go subprocess in background thread
    event_engine = EventEngine()
    t = threading.Thread(target=run_go_process, args=(event_engine,))
    t.daemon = True
    t.start()
    
    # 2. Config
    config = StrategyConfig(
        symbol=os.getenv("SYMBOL", "HYPEUSDT"),
        # ...
    )
    
    # 3. Wait for Go to be ready?
    await asyncio.sleep(2)
    
    # 4. Start Strategy
    # ...
    
    while True:
        await asyncio.sleep(1)

if __name__ == "__main__":
    asyncio.run(main())
