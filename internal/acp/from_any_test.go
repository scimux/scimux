package acp

import (
	"encoding/json"
	"testing"
)

// TestIntFromAny pins existing conversion semantics: supported numeric shapes
// convert; failures and unsupported types yield 0. No new validation policy.
func TestIntFromAny(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want int
	}{
		{"int", int(7), 7},
		{"int64", int64(9), 9},
		{"integral float64", float64(11), 11},
		{"truncating float64", float64(3.9), 3},
		{"integral json.Number", json.Number("13"), 13},
		{"decimal string", "15", 15},
		{"zero int", 0, 0},
		{"negative int", -4, -4},
		{"negative string", "-8", -8},
		{"fractional json.Number", json.Number("2.5"), 0},
		{"invalid json.Number", json.Number("nope"), 0},
		{"invalid string", "x", 0},
		{"unsupported type", true, 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := intFromAny(tc.in); got != tc.want {
				t.Fatalf("intFromAny(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestFloatFromAny pins existing conversion semantics for float-bearing fields.
func TestFloatFromAny(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want float64
	}{
		{"float64", float64(1.5), 1.5},
		{"float32", float32(2.25), float64(float32(2.25))},
		{"int", int(3), 3},
		{"int64", int64(4), 4},
		{"json.Number", json.Number("5.5"), 5.5},
		{"zero", float64(0), 0},
		{"negative", float64(-1.25), -1.25},
		{"fractional", float64(0.125), 0.125},
		{"invalid json.Number", json.Number("nope"), 0},
		{"unsupported type", "1.5", 0},
		{"nil", nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := floatFromAny(tc.in); got != tc.want {
				t.Fatalf("floatFromAny(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
