package autopilot

import (
	"context"
	"fmt"
	"time"

	"github.com/georgehuang/admiral/internal/store"
)

// dailyReportInterval is the teammate's report cadence. First report fires
// one interval after boot — no boot-time noise.
var dailyReportInterval = 24 * time.Hour

// StartDailyReport sends a once-a-day Telegram summary of the teammate's
// last-24h activity. A nil send disables the component entirely (main
// builds the sender and applies the config gate).
func (o *Orchestrator) StartDailyReport(ctx context.Context, send func(text string) error) {
	if send == nil {
		o.logger.Info("daily_report_disabled", "reason", "no sender wired")
		return
	}
	t := time.NewTicker(dailyReportInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			stats, err := o.db.DailyReportStats(time.Now().Add(-dailyReportInterval))
			if err != nil {
				o.logger.Warn("daily_report_stats_failed", "err", err)
				continue
			}
			if err := send(renderDailyReport(stats)); err != nil {
				o.logger.Warn("daily_report_send_failed", "err", err)
			}
		}
	}
}

// renderDailyReport formats the summary. One line per metric so the
// Telegram message stays scannable.
func renderDailyReport(stats *store.DailyReportStats) string {
	return fmt.Sprintf(
		"admiral daily report (last 24h):\n"+
			"· started %d run(s)\n"+
			"· merged %d PR(s)\n"+
			"· review/CI fix rounds: %d\n"+
			"· open PRs awaiting review: %d\n"+
			"· escalated to human: %d",
		stats.StartedRuns, stats.MergedTasks, stats.FixEvents, stats.OpenPRs, stats.FailedTasks)
}
