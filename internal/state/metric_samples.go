package state

import "github.com/cloudground-it/cloudground/internal/api/events"

// metricsKept is how many capacity samples the store holds: a day of the
// metrics.sample job's five-minute readings. The dashboard's chart is that
// day, so nothing older is worth a row.
const metricsKept = 288

// RecordMetric stores one sample and drops whatever falls outside the
// newest metricsKept, in one transaction, then announces it.
func (s *Store) RecordMetric(m MetricSample) error {
	at := now()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO metric_samples (cpu_headroom_pct, mem_headroom_pct, disk_headroom_pct, load1, sampled_at)
		VALUES (?, ?, ?, ?, ?)`, m.CPUHeadroomPct, m.MemHeadroomPct, m.DiskHeadroomPct, m.Load1, at); err != nil {
		return err
	}
	// The cut-off is the newest id that no longer fits; nothing to drop
	// while the table is shorter than that.
	if _, err := tx.Exec(`DELETE FROM metric_samples WHERE id <=
		(SELECT id FROM metric_samples ORDER BY id DESC LIMIT 1 OFFSET ?)`, metricsKept); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	m.SampledAt = parseTime(at)
	s.publish(events.MetricsSample, 0, m)
	return nil
}

// ListMetrics returns the newest n samples (all that are kept when n is not
// between 1 and metricsKept), oldest first, never nil.
func (s *Store) ListMetrics(n int) ([]MetricSample, error) {
	if n < 1 || n > metricsKept {
		n = metricsKept
	}
	rows, err := s.db.Query(`SELECT cpu_headroom_pct, mem_headroom_pct, disk_headroom_pct, load1, sampled_at FROM
		(SELECT * FROM metric_samples ORDER BY id DESC LIMIT ?) ORDER BY id`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MetricSample, 0, n)
	for rows.Next() {
		var m MetricSample
		var at string
		if err := rows.Scan(&m.CPUHeadroomPct, &m.MemHeadroomPct, &m.DiskHeadroomPct, &m.Load1, &at); err != nil {
			return nil, err
		}
		m.SampledAt = parseTime(at)
		out = append(out, m)
	}
	return out, rows.Err()
}
