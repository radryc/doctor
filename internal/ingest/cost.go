package ingest

import (
	"errors"
	"sync/atomic"
)

var ErrCostLimitReached = errors.New("ingest: cost limit reached")

// CostLimits defines configurable limits for object store growth.
type CostLimits struct {
	MaxPendingSegments  int64 // Max total pending segment writes. 0 = unlimited.
	MaxSegmentsPerHour  int64 // Max new segments per hour per tenant. 0 = unlimited.
	MaxTotalObjectBytes int64 // Approximate max total bytes stored. 0 = unlimited.
}

// CostController tracks ingest costs and enforces limits.
type CostController struct {
	limits          CostLimits
	totalSegments   atomic.Int64
	hourlySegments  atomic.Int64
	estimatedBytes  atomic.Int64
	lastHourlyReset atomic.Int64
}

// NewCostController creates a cost controller with the given limits.
func NewCostController(limits CostLimits) *CostController {
	return &CostController{limits: limits}
}

// Check returns an error if writing a segment of the given size would violate limits.
func (cc *CostController) Check(contentSize int64) error {
	if cc.limits.MaxPendingSegments > 0 && cc.totalSegments.Load() >= cc.limits.MaxPendingSegments {
		return ErrCostLimitReached
	}
	if cc.limits.MaxSegmentsPerHour > 0 && cc.hourlySegments.Load() >= cc.limits.MaxSegmentsPerHour {
		return ErrCostLimitReached
	}
	if cc.limits.MaxTotalObjectBytes > 0 && cc.estimatedBytes.Load()+contentSize > cc.limits.MaxTotalObjectBytes {
		return ErrCostLimitReached
	}
	return nil
}

// RecordWrite updates counters after a successful write.
func (cc *CostController) RecordWrite(contentSize int64) {
	cc.totalSegments.Add(1)
	cc.hourlySegments.Add(1)
	cc.estimatedBytes.Add(contentSize)
}

// RecordDelete updates counters after a segment deletion (e.g., compaction or retention).
func (cc *CostController) RecordDelete(contentSize int64) {
	cc.totalSegments.Add(-1)
	if cc.estimatedBytes.Load() >= contentSize {
		cc.estimatedBytes.Add(-contentSize)
	}
}

// ResetHourly resets the hourly segment counter. Called by a periodic tick.
func (cc *CostController) ResetHourly() {
	cc.hourlySegments.Store(0)
}

// Stats returns current cost tracking statistics.
func (cc *CostController) Stats() CostStats {
	return CostStats{
		TotalSegments:  cc.totalSegments.Load(),
		HourlySegments: cc.hourlySegments.Load(),
		EstimatedBytes: cc.estimatedBytes.Load(),
		Limits:         cc.limits,
	}
}

// CostStats holds current cost tracking values.
type CostStats struct {
	TotalSegments  int64      `json:"total_segments"`
	HourlySegments int64      `json:"hourly_segments"`
	EstimatedBytes int64      `json:"estimated_bytes"`
	Limits         CostLimits `json:"limits"`
}
