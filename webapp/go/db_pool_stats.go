package isureal

import (
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"
)

// logDBPoolStats は、接続プールの空き待ちが起きていないかを確かめるため、10秒おきに状態をログに出す。
// WaitCount / WaitDuration は起動からの累計なので、前回からの増分も出す。
func logDBPoolStats(db *sqlx.DB) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	var prevCount int64
	var prevDuration time.Duration
	for range ticker.C {
		stats := db.Stats()
		slog.Info("db pool stats",
			"open", stats.OpenConnections,
			"in_use", stats.InUse,
			"idle", stats.Idle,
			"max_open", stats.MaxOpenConnections,
			"wait_count", stats.WaitCount,
			"wait_count_delta", stats.WaitCount-prevCount,
			"wait_duration_ms", stats.WaitDuration.Milliseconds(),
			"wait_duration_delta_ms", (stats.WaitDuration - prevDuration).Milliseconds(),
		)
		prevCount, prevDuration = stats.WaitCount, stats.WaitDuration
	}
}
