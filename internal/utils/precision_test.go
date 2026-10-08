package utils

import (
	"math"
	"testing"
)

func TestFloorToDecimals(t *testing.T) {
	tests := []struct {
		name      string
		num       float64
		precision int
		expected  float64
	}{
		{"floor 0.666 with 2 decimals", 0.666, 2, 0.66},
		{"floor 0.669 with 2 decimals", 0.669, 2, 0.66},
		{"floor 0.661 with 2 decimals", 0.661, 2, 0.66},
		{"floating point IEEE 754 2.53", 2.53, 2, 2.53},
		{"integer precision 0", 12.99, 0, 12.0},
		{"zero", 0.0, 2, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FloorToDecimals(tt.num, tt.precision)
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("FloorToDecimals(%v, %v) = %v; want %v", tt.num, tt.precision, got, tt.expected)
			}
		})
	}
}

func TestFloorToTickSize(t *testing.T) {
	tests := []struct {
		name     string
		num      float64
		tickSize float64
		expected float64
	}{
		{"floor 0.1666 by 0.01", 0.1666, 0.01, 0.16},
		{"floor 0.1699 by 0.01", 0.1699, 0.01, 0.16},
		{"exact multiple 0.15 by 0.01", 0.15, 0.01, 0.15},
		{"zero tickSize returns num", 0.1666, 0.0, 0.1666},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FloorToTickSize(tt.num, tt.tickSize)
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("FloorToTickSize(%v, %v) = %v; want %v", tt.num, tt.tickSize, got, tt.expected)
			}
		})
	}
}
