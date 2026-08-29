package loadgen

import "math"

// Small wrappers so the atomic float dance and the one log call read clearly at
// their use sites rather than sprawling math.Float64bits across the logic.

func mathFloatBits(f float64) uint64     { return math.Float64bits(f) }
func mathFloatFromBits(u uint64) float64 { return math.Float64frombits(u) }
func mathLog(f float64) float64          { return math.Log(f) }
