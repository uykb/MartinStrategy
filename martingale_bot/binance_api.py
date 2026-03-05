import asyncio
import hashlib
import hmac
import time
import json
import logging
import urllib.parse
from typing import Dict, List, Optional, Any
from enum import Enum
import aiohttp
from .data_types import OrderSide, OrderType, OrderStatus, OrderData, PositionData, TickData

# 配置日志
logger = logging.getLogger(__name__)

class BinanceClient:
    """
    Binance USDT-Margined Futures API Client (Async)
    官方文档: https://binance-docs.github.io/apidocs/futures/en/
    """
    
    BASE_URL = "https://fapi.binance.com" # 生产环境
    # BASE_URL = "https://testnet.binancefuture.com" # 测试环境 (可选)
    
    def __init__(self, api_key: str, api_secret: str, testnet: bool = False):
        self.api_key = api_key
        self.api_secret = api_secret
        self.testnet = testnet
        if testnet:
            self.BASE_URL = "https://testnet.binancefuture.com"
            
        self.session = None

    async def _init_session(self):
        if not self.session:
            self.session = aiohttp.ClientSession()

    async def _close_session(self):
        if self.session:
            await self.session.close()

    def _generate_signature(self, query_string: str) -> str:
        return hmac.new(
            self.api_secret.encode('utf-8'),
            query_string.encode('utf-8'),
            hashlib.sha256
        ).hexdigest()

    async def _request(self, method: str, endpoint: str, params: Dict = None, signed: bool = False):
        await self._init_session()
        
        url = f"{self.BASE_URL}{endpoint}"
        params = params or {}
        
        if signed:
            params['timestamp'] = int(time.time() * 1000)
            query_string = urllib.parse.urlencode(params)
            signature = self._generate_signature(query_string)
            params['signature'] = signature

        headers = {
            'X-MBX-APIKEY': self.api_key,
            'Content-Type': 'application/json'
        }

        try:
            async with self.session.request(method, url, params=params, headers=headers) as response:
                if response.status == 200:
                    return await response.json()
                else:
                    text = await response.text()
                    logger.error(f"Binance API Error [{response.status}]: {text}")
                    # 可以抛出自定义异常
                    return None
        except Exception as e:
            logger.error(f"Request failed: {e}")
            return None

    # --- Market Data ---

    async def get_klines(self, symbol: str, interval: str = '15m', limit: int = 50) -> List[Dict]:
        """
        获取K线数据
        GET /fapi/v1/klines
        """
        params = {
            'symbol': symbol.replace('/', ''), # BTC/USDT -> BTCUSDT
            'interval': interval,
            'limit': limit
        }
        return await self._request('GET', '/fapi/v1/klines', params=params)

    async def get_symbol_price(self, symbol: str) -> float:
        """
        获取最新价格
        GET /fapi/v1/ticker/price
        """
        params = {'symbol': symbol.replace('/', '')}
        res = await self._request('GET', '/fapi/v1/ticker/price', params=params)
        if res:
            return float(res['price'])
        return 0.0

    # --- Account & Trade ---

    async def get_position_risk(self, symbol: str) -> Optional[PositionData]:
        """
        获取特定币种的持仓风险
        GET /fapi/v2/positionRisk
        """
        symbol_fmt = symbol.replace('/', '')
        res = await self._request('GET', '/fapi/v2/positionRisk', params={'symbol': symbol_fmt}, signed=True)
        
        if res:
            # 找到对应 symbol 的持仓 (API 可能返回列表)
            for pos in res:
                if pos['symbol'] == symbol_fmt:
                    amt = float(pos['positionAmt'])
                    entry_price = float(pos['entryPrice'])
                    unrealized_pnl = float(pos['unRealizedProfit'])
                    
                    return PositionData(
                        symbol=symbol,
                        quantity=abs(amt), # 策略层通常用正数表示数量，方向由 side 决定? 或者这里保留正负? 
                        # 简单起见，我们假设是单向持仓或净持仓。
                        # 如果 amt > 0 多头，amt < 0 空头。
                        # 为了兼容现有的 data_types，我们需要确认 quantity 定义。
                        # 假设 data_types.PositionData 用于多头策略:
                        average_price=entry_price,
                        current_price=float(pos['markPrice']), # 使用标记价格更安全
                        unrealized_pnl=unrealized_pnl
                    )
        return None

    async def create_order(self, symbol: str, side: OrderSide, type: OrderType, quantity: float, price: float = None, time_in_force: str = "GTC") -> Optional[OrderData]:
        """
        下单
        POST /fapi/v1/order
        """
        params = {
            'symbol': symbol.replace('/', ''),
            'side': side.value,
            'type': type.value,
            'quantity': quantity,
        }
        
        if type == OrderType.LIMIT:
            if price is None:
                logger.error("Limit order requires price")
                return None
            params['price'] = price
            params['timeInForce'] = time_in_force

        res = await self._request('POST', '/fapi/v1/order', params=params, signed=True)
        
        if res:
            return OrderData(
                symbol=symbol,
                order_id=str(res['orderId']),
                side=side,
                type=type,
                price=float(res.get('price', 0)) if type == OrderType.LIMIT else 0.0, # 市价单可能没有 price
                quantity=float(res['origQty']),
                filled_quantity=float(res['executedQty']),
                status=OrderStatus(res['status']),
                tag="" # Tag 需要外部管理，API 不存储自定义 tag (或者可以用 clientOrderId)
            )
        return None

    async def cancel_order(self, symbol: str, order_id: str):
        """
        撤单
        DELETE /fapi/v1/order
        """
        params = {
            'symbol': symbol.replace('/', ''),
            'orderId': order_id
        }
        return await self._request('DELETE', '/fapi/v1/order', params=params, signed=True)

    async def cancel_all_orders(self, symbol: str):
        """
        撤销某交易对所有挂单
        DELETE /fapi/v1/allOpenOrders
        """
        params = {
            'symbol': symbol.replace('/', '')
        }
        return await self._request('DELETE', '/fapi/v1/allOpenOrders', params=params, signed=True)

    async def get_open_orders(self, symbol: str) -> List[OrderData]:
        """
        获取当前挂单
        GET /fapi/v1/openOrders
        """
        params = {'symbol': symbol.replace('/', '')}
        res = await self._request('GET', '/fapi/v1/openOrders', params=params, signed=True)
        
        orders = []
        if res:
            for o in res:
                orders.append(OrderData(
                    symbol=symbol,
                    order_id=str(o['orderId']),
                    side=OrderSide(o['side']),
                    type=OrderType(o['type']),
                    price=float(o['price']),
                    quantity=float(o['origQty']),
                    filled_quantity=float(o['executedQty']),
                    status=OrderStatus(o['status'])
                ))
        return orders
