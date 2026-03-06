package utils

import (
	"math"

	"github.com/markcheno/go-talib"
)

// CalculateATR calculates the Average True Range
// klines: slice of High, Low, Close prices
// period: typical value is 14
func CalculateATR(highs, lows, closes []float64, period int) float64 {
	if len(highs) < period+1 {
		return 0
	}

	atr := talib.Atr(highs, lows, closes, period)
	if len(atr) == 0 {
		return 0
	}
	
	// Return the latest ATR value
	return atr[len(atr)-1]
}

// Convert float64 slice to precision for orders
func ToFixed(num float64, precision int) float64 {
	output := math.Pow(10, float64(precision))
	return float64(int(num*output)) / output
}
