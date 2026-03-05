import pandas as pd
import pandas_ta as ta
from typing import List, Dict

class TechnicalAnalysis:
    """
    技术分析工具类
    """
    @staticmethod
    def calculate_atr(klines: List[Dict], period: int = 14) -> float:
        """
        计算 ATR (Average True Range)
        klines: Binance K线数据格式 [[time, open, high, low, close, vol...], ...]
        """
        if not klines or len(klines) < period:
            return 0.0
            
        # 转换为 DataFrame
        # Binance kline 格式:
        # 0: Open time, 1: Open, 2: High, 3: Low, 4: Close, 5: Volume ...
        df = pd.DataFrame(klines, columns=[
            'timestamp', 'open', 'high', 'low', 'close', 'volume', 
            'close_time', 'quote_asset_volume', 'trades', 'buy_base_vol', 'buy_quote_vol', 'ignore'
        ])
        
        # 转换数据类型
        df['high'] = df['high'].astype(float)
        df['low'] = df['low'].astype(float)
        df['close'] = df['close'].astype(float)
        
        # 计算 ATR
        # 使用 pandas_ta
        try:
            atr_series = df.ta.atr(length=period)
            if atr_series is not None and not atr_series.empty:
                return float(atr_series.iloc[-1]) # 返回最新的 ATR
        except Exception as e:
            print(f"Error calculating ATR: {e}")
            
        return 0.0
