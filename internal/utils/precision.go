package utils

import (
	"math"
)

// ToFixed converts a float64 to a specified precision for exchange formatting.
func ToFixed(num float64, precision int) float64 {
	output := math.Pow(10, float64(precision))
	return math.Round(num*output) / output
}

// FloorToDecimals 将数量向下取整到指定小数位数（Floor truncation）。
// 用于代币数量（Size）的计算，确保：
//   - 不会因四舍五入/向上取整导致余额不足（Insufficient Funds）被拒单
//   - 平仓数量 ≤ 实际持仓量，不会产生反向微型尾仓
func FloorToDecimals(num float64, precision int) float64 {
	output := math.Pow(10, float64(precision))
	return math.Floor(num*output+0.00000001) / output
}

// FloorToTickSize 将数量向下取整到 tickSize 的整数倍。
func FloorToTickSize(num float64, tickSize float64) float64 {
	if tickSize == 0 {
		return num
	}
	return math.Floor(num/tickSize+0.00000001) * tickSize
}

// RoundUpToTickSize rounds up a number to the nearest multiple of tickSize.
// Example: num=0.1666, tickSize=0.01 -> 0.17
func RoundUpToTickSize(num float64, tickSize float64) float64 {
	if tickSize == 0 {
		return num
	}
	return math.Ceil(num/tickSize-0.00000001) * tickSize
}

// RoundToTickSize rounds a number to the nearest multiple of tickSize (standard rounding).
// Used for price formatting to match exchange filters.
func RoundToTickSize(num float64, tickSize float64) float64 {
	if tickSize == 0 {
		return num
	}
	return math.Round(num/tickSize) * tickSize
}
