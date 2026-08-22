package utils

import (
	"math"
)

// ToFixed converts a float64 to a specified precision for exchange formatting.
func ToFixed(num float64, precision int) float64 {
	output := math.Pow(10, float64(precision))
	return math.Round(num*output) / output
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
