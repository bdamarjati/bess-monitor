package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"
)

// Retention is how long each tier is kept.
type Retention struct {
	Raw, Minute, Hour time.Duration
}

var DefaultRetention = Retention{Raw: 7 * 24 * time.Hour, Minute: 90 * 24 * time.Hour, Hour: 1825 * 24 * time.Hour}

// tier describes one rollup step: src rows are aggregated into dst buckets of size seconds.
type tier struct {
	src, dst string
	size     int64
	// lookback: recent buckets re-aggregated on each run, so late writes
	// (e.g. a retried batch) are still included. Rows are replaced, never duplicated.
	lookback int64
}

var (
	minuteTier = tier{src: "readings", dst: "readings_1m", size: 60, lookback: 15 * 60}
	hourTier   = tier{src: "readings_1m", dst: "readings_1h", size: 3600, lookback: 2 * 3600}
)

const (
	minuteEvery  = 5 * time.Minute
	hourEvery    = 10 * time.Minute // a multiple of minuteEvery
	cleanupEvery = 24 * time.Hour
	// settle: a minute counts as completed only once it ended this long ago,
	// so the 10 s batch flush has written its readings.
	settle = 30 * time.Second
)

// deviceList is a recursive CTE named name(id) listing distinct device ids in table
// by hopping through the (device_id, ts) index: one lookup per device instead of a
// full scan. The last row is NULL; filter with "id IS NOT NULL".
func deviceList(name, table string) string {
	return fmt.Sprintf(`%[1]s(id) AS (
		SELECT min(device_id) FROM %[2]s
		UNION ALL
		SELECT (SELECT min(device_id) FROM %[2]s WHERE device_id > %[1]s.id) FROM %[1]s WHERE %[1]s.id IS NOT NULL)`,
		name, table)
}

// deviceIDs is a WITH clause defining dev(id) for table.
func deviceIDs(table string) string {
	return "WITH RECURSIVE " + deviceList("dev", table) + " "
}

// completed returns the end of the last completed bucket of size seconds at time now.
func completed(now time.Time, size int64) int64 {
	return now.Add(-settle).Unix() / size * size
}

// RollupMinutes aggregates every completed minute of raw readings not yet in readings_1m.
func (s *Store) RollupMinutes(ctx context.Context, now time.Time) (int64, error) {
	return s.rollup(ctx, minuteTier, completed(now, 60))
}

// RollupHours aggregates every completed hour of readings_1m not yet in readings_1h.
// Run RollupMinutes first so the hour's minutes are all present.
func (s *Store) RollupHours(ctx context.Context, now time.Time) (int64, error) {
	return s.rollup(ctx, hourTier, completed(now, 3600))
}

// rollup catches up each device from its newest dst bucket (or its oldest src row)
// up to, but not including, the bucket starting at to.
func (s *Store) rollup(ctx context.Context, t tier, to int64) (int64, error) {
	devices, err := s.deviceStarts(ctx, t)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, d := range devices {
		from := max(d.start-t.lookback, d.oldest) / t.size * t.size
		n, err := s.aggregate(ctx, t, d.id, from, to)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

type deviceStart struct {
	id            string
	start, oldest int64
}

// deviceStarts returns, per device in t.src, where its rollup should resume:
// just after its newest dst bucket, or at its oldest src row if it has none.
func (s *Store) deviceStarts(ctx context.Context, t tier) ([]deviceStart, error) {
	rows, err := s.db.QueryContext(ctx, deviceIDs(t.src)+fmt.Sprintf(`
		SELECT id,
			(SELECT max(ts) FROM %[2]s WHERE device_id = dev.id),
			(SELECT min(ts) FROM %[1]s WHERE device_id = dev.id)
		FROM dev WHERE id IS NOT NULL`, t.src, t.dst))
	if err != nil {
		return nil, fmt.Errorf("rollup %s: devices: %w", t.dst, err)
	}
	defer rows.Close()

	var out []deviceStart
	for rows.Next() {
		var d deviceStart
		var newest sql.NullInt64
		if err := rows.Scan(&d.id, &newest, &d.oldest); err != nil {
			return nil, fmt.Errorf("rollup %s: scan: %w", t.dst, err)
		}
		d.start = d.oldest
		if newest.Valid {
			d.start = newest.Int64 + t.size
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// aggregate writes dst buckets for one device over [from, to). Status and error_code
// take the max so a fault is never averaged away; empty buckets produce no row.
func (s *Store) aggregate(ctx context.Context, t tier, device string, from, to int64) (int64, error) {
	if from >= to {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, fmt.Sprintf(`
		INSERT OR REPLACE INTO %[2]s (
			device_id, ts, status, error_code,
			bat_soc, bat_soh, bat_voltage, bat_current, cell_min_mv, cell_max_mv,
			solar_voltage, solar_current, solar_power,
			grid_voltage, grid_current, grid_power, grid_freq,
			load_voltage, load_current, load_power, load_freq)
		SELECT device_id, (ts / ?1) * ?1 AS b, max(status), max(error_code),
			avg(bat_soc), avg(bat_soh), avg(bat_voltage), avg(bat_current), min(cell_min_mv), max(cell_max_mv),
			avg(solar_voltage), avg(solar_current), avg(solar_power),
			avg(grid_voltage), avg(grid_current), avg(grid_power), avg(grid_freq),
			avg(load_voltage), avg(load_current), avg(load_power), avg(load_freq)
		FROM %[1]s
		WHERE device_id = ?2 AND ts >= ?3 AND ts < ?4
		GROUP BY device_id, b`, t.src, t.dst), t.size, device, from, to)
	if err != nil {
		return 0, fmt.Errorf("rollup %s for %s: %w", t.dst, device, err)
	}
	return res.RowsAffected()
}

// Cleanup deletes rows past their retention. Raw and minute rows are deleted only
// when their bucket already exists in the next tier, so nothing is lost un-rolled-up.
func (s *Store) Cleanup(ctx context.Context, now time.Time, r Retention) (raw, minute, hour int64, err error) {
	if raw, err = s.deleteRolledUp(ctx, minuteTier, now.Add(-r.Raw).Unix()); err != nil {
		return
	}
	if minute, err = s.deleteRolledUp(ctx, hourTier, now.Add(-r.Minute).Unix()); err != nil {
		return
	}
	res, err := s.db.ExecContext(ctx, deviceIDs("readings_1h")+
		`DELETE FROM readings_1h WHERE device_id IN (SELECT id FROM dev WHERE id IS NOT NULL) AND ts < ?`,
		now.Add(-r.Hour).Unix())
	if err != nil {
		return raw, minute, 0, fmt.Errorf("cleanup readings_1h: %w", err)
	}
	hour, err = res.RowsAffected()
	return
}

// deleteRolledUp deletes t.src rows older than cutoff whose bucket exists in t.dst.
// The cutoff is aligned to a bucket start so only whole buckets are removed; a
// later re-aggregation can then never overwrite a bucket with partial data.
func (s *Store) deleteRolledUp(ctx context.Context, t tier, cutoff int64) (int64, error) {
	cutoff = cutoff / t.size * t.size
	res, err := s.db.ExecContext(ctx, deviceIDs(t.src)+fmt.Sprintf(`
		DELETE FROM %[1]s
		WHERE device_id IN (SELECT id FROM dev WHERE id IS NOT NULL) AND ts < ?2
		AND EXISTS (SELECT 1 FROM %[2]s r WHERE r.device_id = %[1]s.device_id AND r.ts = (%[1]s.ts / ?1) * ?1)`,
		t.src, t.dst), t.size, cutoff)
	if err != nil {
		return 0, fmt.Errorf("cleanup %s: %w", t.src, err)
	}
	return res.RowsAffected()
}

// Maintain runs the minute rollup every 5 minutes, the hour rollup every 10 minutes
// and cleanup daily, until ctx is cancelled. The first run (at startup) catches up
// on everything missed while the app was down.
func (s *Store) Maintain(ctx context.Context, r Retention) {
	var lastHour, lastCleanup time.Time
	run := func(first bool) {
		now := time.Now()
		minutes, err := s.RollupMinutes(ctx, now)
		if err != nil {
			logErr(ctx, "minute rollup", err)
			return // hour rollup and cleanup depend on it
		}
		var hours int64
		if now.Sub(lastHour) >= hourEvery-time.Minute { // tolerate ticker jitter
			if hours, err = s.RollupHours(ctx, now); err != nil {
				logErr(ctx, "hour rollup", err)
				return
			}
			lastHour = now
		}
		if first {
			slog.Info("rollup caught up", "minutes", minutes, "hours", hours)
		}
		if now.Sub(lastCleanup) >= cleanupEvery {
			raw, minute, hour, err := s.Cleanup(ctx, now, r)
			if err != nil {
				logErr(ctx, "cleanup", err)
				return
			}
			lastCleanup = now
			slog.Info("cleanup", "raw_deleted", raw, "1m_deleted", minute, "1h_deleted", hour)
			if _, err := s.db.ExecContext(ctx, "PRAGMA optimize"); err != nil {
				logErr(ctx, "optimize", err)
			}
		}
	}

	run(true)
	t := time.NewTicker(minuteEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			run(false)
		}
	}
}

// logErr logs maintenance errors, except those caused by shutdown.
func logErr(ctx context.Context, what string, err error) {
	if ctx.Err() == nil {
		slog.Error(what+" failed", "err", err)
	}
}
