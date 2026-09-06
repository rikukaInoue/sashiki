// capacity: storage watermark / quota と capacity 表示(仕様 14-2, 14-4)。
package workspace

import (
	"context"
	"fmt"

	"github.com/rikukaInoue/sashiki/internal/storage"
)

// Capacity は memory / storage / ports の空き状況(GET /capacity)。
type Capacity struct {
	// storage
	PoolUsedBytes  int64
	PoolTotalBytes int64
	PoolUsedRatio  float64
	HighWatermark  float64
	CritWatermark  float64
	// ports
	PortsUsed  int
	PortsTotal int
	// memory
	MemAvailableBytes int64
	ExpectedRSSBytes  int64
	// branches
	Running     int
	MaxRunning  int
	MaxBranches int
}

// Capacity は現在の空き状況を返す。
func (m *Manager) Capacity(ctx context.Context) (Capacity, error) {
	c := Capacity{
		HighWatermark:    m.cfg.HighWatermark,
		CritWatermark:    m.cfg.CriticalWatermark,
		PortsTotal:       m.cfg.PortHigh - m.cfg.PortLow + 1,
		ExpectedRSSBytes: m.expectedRSS(),
		MaxRunning:       m.cfg.MaxRunning,
		MaxBranches:      m.cfg.MaxBranches,
	}
	if cr, ok := m.st.(storage.CapacityReporter); ok {
		if used, total, err := cr.PoolCapacity(ctx); err == nil && total > 0 {
			c.PoolUsedBytes = used
			c.PoolTotalBytes = total
			c.PoolUsedRatio = float64(used) / float64(total)
		}
	}
	if used, err := m.db.UsedPorts(); err == nil {
		c.PortsUsed = len(used)
	}
	if m.cfg.AvailableMem != nil {
		if avail, err := m.cfg.AvailableMem(); err == nil {
			c.MemAvailableBytes = avail
		}
	}
	if branches, err := m.db.ListBranches(); err == nil {
		for _, b := range branches {
			if b.State == "running" {
				c.Running++
			}
		}
	}
	return c, nil
}

// admitStorage は critical watermark を超えていたら操作を拒否する(仕様 14-2)。
func (m *Manager) admitStorage(ctx context.Context, op string) error {
	if m.cfg.CriticalWatermark <= 0 {
		return nil
	}
	cr, ok := m.st.(storage.CapacityReporter)
	if !ok {
		return nil
	}
	used, total, err := cr.PoolCapacity(ctx)
	if err != nil || total <= 0 {
		return nil // 判定不能なら通す(best-effort)
	}
	ratio := float64(used) / float64(total)
	if ratio >= m.cfg.CriticalWatermark {
		return fmt.Errorf("%w: storage (pool %.0f%% >= critical %.0f%%, op=%s)",
			ErrLimitReached, ratio*100, m.cfg.CriticalWatermark*100, op)
	}
	return nil
}
