package utils

import (
	"math"
)

// Convert float64 slice to precision for orders
func ToFixed(num float64, precision int) float64 {
	output := math.Pow(10, float64(precision))
	return math.Round(num*output) / output
}

// RoundUpToTickSize rounds up a number to the nearest multiple of tickSize
// Example: num=0.1666, tickSize=0.01 -> 0.17
func RoundUpToTickSize(num float64, tickSize float64) float64 {
	if tickSize == 0 {
		return num
	}
	// Use Decimal for precision if needed, but for now simple float math with epsilon
	// ceil(num / tickSize) * tickSize
	// Adding epsilon to handle float precision issues
	return math.Ceil(num/tickSize - 0.00000001) * tickSize
}

// RoundToTickSize rounds a number to the nearest multiple of tickSize (standard rounding)
// Used for price formatting to match exchange filters
func RoundToTickSize(num float64, tickSize float64) float64 {
	if tickSize == 0 {
		return num
	}
	return math.Round(num/tickSize) * tickSize
}
