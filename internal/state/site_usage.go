package state

import (
	"time"
)

// Site usage (#18, api SPEC §11.5): one row per site every five minutes for
// the last day, and one row per site and hour for the last 30 days, kept
// in step as samples are written. Nothing grows past that.

// UsageSample is one reading of a site's slice. The raw counters (CPU time
// and IO bytes since the slice started) are kept so the next sample can take
// differences; the rates are over the time since the previous sample.
type UsageSample struct {
	SiteID       int64     `json:"site_id"`
	SampledAt    time.Time `json:"sampled_at"`
	CPUUsec      int64     `json:"-"`
	IOReadBytes  int64     `json:"-"`
	IOWriteBytes int64     `json:"-"`
	// CPUCores is the CPU time used per second: 1.0 is one CPU busy.
	CPUCores    float64 `json:"cpu_cores"`
	MemBytes    int64   `json:"mem_bytes"`
	MemMaxBytes int64   `json:"mem_max_bytes"`
	IOReadBps   float64 `json:"io_read_bps"`
	IOWriteBps  float64 `json:"io_write_bps"`
	Tasks       int64   `json:"tasks"`
	OOMKills    int64   `json:"oom_kills"`
	// DiskBytes and DiskInodes are -1 when unknown.
	DiskBytes  int64 `json:"disk_bytes"`
	DiskInodes int64 `json:"disk_inodes"`
}

// UsageHour is the hourly summary of a site's samples.
type UsageHour struct {
	Hour        time.Time `json:"hour"`
	CPUCores    float64   `json:"cpu_cores"` // the hour's average
	CPUCoresMax float64   `json:"cpu_cores_max"`
	MemBytes    int64     `json:"mem_bytes"` // the hour's average
	MemBytesMax int64     `json:"mem_bytes_max"`
	MemMaxBytes int64     `json:"mem_max_bytes"`
	IOReadBps   float64   `json:"io_read_bps"`
	IOWriteBps  float64   `json:"io_write_bps"`
	DiskBytes   int64     `json:"disk_bytes"`
}

// How long each table keeps its rows.
const (
	UsageSampleKeep = 24 * time.Hour
	UsageHourKeep   = 30 * 24 * time.Hour
)

// usageStamp is t in a fixed-width form, so the times of these tables sort
// as text.
func usageStamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z07:00")
}

// RecordUsage stores one round of samples, folds each into its hour, and
// drops what fell out of either window, in one transaction.
func (s *Store) RecordUsage(samples []UsageSample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, u := range samples {
		at := u.SampledAt
		if at.IsZero() {
			at = time.Now()
		}
		if _, err := tx.Exec(`INSERT INTO site_usage (site_id, sampled_at, cpu_usec, io_read_bytes, io_write_bytes,
			cpu_cores, mem_bytes, mem_max_bytes, io_read_bps, io_write_bps, tasks, oom_kills, disk_bytes, disk_inodes)
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM sites WHERE id = ?)`,
			u.SiteID, usageStamp(at), u.CPUUsec, u.IOReadBytes, u.IOWriteBytes, u.CPUCores, u.MemBytes, u.MemMaxBytes,
			u.IOReadBps, u.IOWriteBps, u.Tasks, u.OOMKills, u.DiskBytes, u.DiskInodes, u.SiteID); err != nil {
			return err
		}
		hour := usageStamp(at.UTC().Truncate(time.Hour))
		if _, err := tx.Exec(`INSERT INTO site_usage_hourly (site_id, hour, n, cpu_cores_sum, cpu_cores_max, mem_bytes_sum,
			mem_bytes_max, mem_max_bytes, io_read_sum, io_write_sum, disk_bytes)
			SELECT ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, ? WHERE EXISTS (SELECT 1 FROM sites WHERE id = ?)
			ON CONFLICT (site_id, hour) DO UPDATE SET n = n + 1,
				cpu_cores_sum = cpu_cores_sum + excluded.cpu_cores_sum,
				cpu_cores_max = max(cpu_cores_max, excluded.cpu_cores_max),
				mem_bytes_sum = mem_bytes_sum + excluded.mem_bytes_sum,
				mem_bytes_max = max(mem_bytes_max, excluded.mem_bytes_max),
				mem_max_bytes = excluded.mem_max_bytes,
				io_read_sum = io_read_sum + excluded.io_read_sum,
				io_write_sum = io_write_sum + excluded.io_write_sum,
				disk_bytes = CASE WHEN excluded.disk_bytes >= 0 THEN excluded.disk_bytes ELSE disk_bytes END`,
			u.SiteID, hour, u.CPUCores, u.CPUCores, float64(u.MemBytes), u.MemBytes, u.MemMaxBytes,
			u.IOReadBps, u.IOWriteBps, u.DiskBytes, u.SiteID); err != nil {
			return err
		}
	}
	now := time.Now()
	if _, err := tx.Exec(`DELETE FROM site_usage WHERE sampled_at < ?`, usageStamp(now.Add(-UsageSampleKeep))); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM site_usage_hourly WHERE hour < ?`, usageStamp(now.Add(-UsageHourKeep))); err != nil {
		return err
	}
	return tx.Commit()
}

const usageCols = `site_id, sampled_at, cpu_usec, io_read_bytes, io_write_bytes, cpu_cores, mem_bytes, mem_max_bytes,
	io_read_bps, io_write_bps, tasks, oom_kills, disk_bytes, disk_inodes`

func scanUsage(sc interface{ Scan(...any) error }) (UsageSample, error) {
	var u UsageSample
	var at string
	err := sc.Scan(&u.SiteID, &at, &u.CPUUsec, &u.IOReadBytes, &u.IOWriteBytes, &u.CPUCores, &u.MemBytes, &u.MemMaxBytes,
		&u.IOReadBps, &u.IOWriteBps, &u.Tasks, &u.OOMKills, &u.DiskBytes, &u.DiskInodes)
	u.SampledAt = parseTime(at)
	return u, err
}

// UsageSince returns every site's samples taken after since, oldest first,
// by site.
func (s *Store) UsageSince(since time.Time) (map[int64][]UsageSample, error) {
	rows, err := s.db.Query(`SELECT `+usageCols+` FROM site_usage WHERE sampled_at > ? ORDER BY site_id, sampled_at`, usageStamp(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]UsageSample{}
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		out[u.SiteID] = append(out[u.SiteID], u)
	}
	return out, rows.Err()
}

// SiteUsageSince is one site's samples after since, oldest first, never nil.
func (s *Store) SiteUsageSince(siteID int64, since time.Time) ([]UsageSample, error) {
	rows, err := s.db.Query(`SELECT `+usageCols+` FROM site_usage WHERE site_id = ? AND sampled_at > ? ORDER BY sampled_at`,
		siteID, usageStamp(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageSample{}
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// LatestUsage is each site's newest sample.
func (s *Store) LatestUsage() (map[int64]UsageSample, error) {
	rows, err := s.db.Query(`SELECT ` + usageCols + ` FROM site_usage u WHERE id = (SELECT max(id) FROM site_usage WHERE site_id = u.site_id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]UsageSample{}
	for rows.Next() {
		u, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		out[u.SiteID] = u
	}
	return out, rows.Err()
}

// SiteUsageHours is one site's hourly rows after since, oldest first,
// never nil.
func (s *Store) SiteUsageHours(siteID int64, since time.Time) ([]UsageHour, error) {
	rows, err := s.db.Query(`SELECT hour, cpu_cores_sum / n, cpu_cores_max, mem_bytes_sum / n, mem_bytes_max, mem_max_bytes,
		io_read_sum / n, io_write_sum / n, disk_bytes FROM site_usage_hourly WHERE site_id = ? AND hour > ? ORDER BY hour`,
		siteID, usageStamp(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UsageHour{}
	for rows.Next() {
		var h UsageHour
		var at string
		var mem float64
		if err := rows.Scan(&at, &h.CPUCores, &h.CPUCoresMax, &mem, &h.MemBytesMax, &h.MemMaxBytes, &h.IOReadBps, &h.IOWriteBps, &h.DiskBytes); err != nil {
			return nil, err
		}
		h.Hour, h.MemBytes = parseTime(at), int64(mem)
		out = append(out, h)
	}
	return out, rows.Err()
}
