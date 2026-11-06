package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"bess-monitor/internal/ingest"
)

// ErrNotFound is returned when a device has no data.
var ErrNotFound = errors.New("not found")

type Device struct {
	ID     string `json:"device_id"`
	LastTS int64  `json:"last_ts"`
}

// Devices lists every known device with the time of its newest reading.
func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	// Index hops per device (see deviceList) instead of scanning every row.
	rows, err := s.db.QueryContext(ctx, `WITH RECURSIVE `+
		deviceList("dr", "readings")+`, `+deviceList("dm", "readings_1m")+`, `+deviceList("dh", "readings_1h")+`
		SELECT id, max(ts) FROM (
			SELECT id, (SELECT max(ts) FROM readings WHERE device_id = dr.id) AS ts FROM dr WHERE id IS NOT NULL
			UNION ALL
			SELECT id, (SELECT max(ts) FROM readings_1m WHERE device_id = dm.id) FROM dm WHERE id IS NOT NULL
			UNION ALL
			SELECT id, (SELECT max(ts) FROM readings_1h WHERE device_id = dh.id) FROM dh WHERE id IS NOT NULL
		) GROUP BY id ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query devices: %w", err)
	}
	defer rows.Close()

	out := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.LastTS); err != nil {
			return nil, fmt.Errorf("scan device: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Latest returns the newest raw reading for a device.
func (s *Store) Latest(ctx context.Context, deviceID string) (ingest.Reading, error) {
	var (
		r            ingest.Reading
		b            = &r.Battery
		temps, cells sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT device_id, ts, status, error_code,
			bat_soc, bat_soh, bat_voltage, bat_current, bat_temps, bat_cells,
			solar_voltage, solar_current, solar_power,
			grid_voltage, grid_current, grid_power, grid_freq,
			load_voltage, load_current, load_power, load_freq
		FROM readings WHERE device_id = ? ORDER BY ts DESC, id DESC LIMIT 1`, deviceID).Scan(
		&r.DeviceID, &r.TS, &r.Status, &b.ErrorCode,
		&b.SoC, &b.SoH, &b.Voltage, &b.Current, &temps, &cells,
		&r.Solar.Voltage, &r.Solar.Current, &r.Solar.Power,
		&r.Grid.Voltage, &r.Grid.Current, &r.Grid.Power, &r.Grid.Freq,
		&r.Load.Voltage, &r.Load.Current, &r.Load.Power, &r.Load.Freq,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, fmt.Errorf("query latest: %w", err)
	}
	if b.Temperature, err = parseArray(temps); err != nil {
		return r, err
	}
	if b.CellVoltage, err = parseArray(cells); err != nil {
		return r, err
	}
	return r, nil
}

func parseArray(s sql.NullString) ([]float64, error) {
	if !s.Valid {
		return nil, nil
	}
	var v []float64
	if err := json.Unmarshal([]byte(s.String), &v); err != nil {
		return nil, fmt.Errorf("parse stored array: %w", err)
	}
	return v, nil
}

// Series is columnar history in uPlot format. Nil values mark gaps.
type Series struct {
	TS         []int64    `json:"ts"`
	BatSoC     []*float64 `json:"bat_soc"`
	BatCurrent []*float64 `json:"bat_current"`
	BatVoltage []*float64 `json:"bat_voltage"`
	SolarPower []*float64 `json:"solar_power"`
	GridPower  []*float64 `json:"grid_power"`
	LoadPower  []*float64 `json:"load_power"`
	CellMinMV  []*float64 `json:"cell_min_mv"`
	CellMaxMV  []*float64 `json:"cell_max_mv"`
}

// Source tables by requested range, finest first. gap is the smallest hole
// (seconds) shown as a break in the line; it must exceed the table's resolution.
var sources = []struct {
	table    string
	maxRange int64 // longest range served from this table; 0 = no limit
	gap      int64
}{
	{"readings", 24 * 3600, 60},
	{"readings_1m", 90 * 24 * 3600, 60},
	{"readings_1h", 0, 3600},
}

// History returns data for [from, to) as at most maxPoints points. If the source
// table has no more than maxPoints rows in the range they are returned as-is;
// otherwise they are averaged into equal time buckets.
func (s *Store) History(ctx context.Context, deviceID string, from, to int64, maxPoints int) (*Series, error) {
	if to <= from || maxPoints < 2 {
		return nil, fmt.Errorf("invalid range")
	}
	src := sources[len(sources)-1]
	for _, c := range sources {
		if to-from <= c.maxRange {
			src = c
			break
		}
	}

	var n int
	var first, last sql.NullInt64
	err := s.db.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT count(*), min(ts), max(ts) FROM %s WHERE device_id = ? AND ts >= ? AND ts < ?`, src.table),
		deviceID, from, to).Scan(&n, &first, &last)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	if n == 0 {
		return newSeries(), nil
	}
	bucket := int64(1) // one row per point (rows in a rollup table have unique ts)
	if n > maxPoints {
		bucket = bucketSize(last.Int64-first.Int64+1, maxPoints)
	}

	// Bucket starts are aligned to multiples of bucket, so the data span fits in maxPoints buckets.
	q := fmt.Sprintf(`
		SELECT (ts / ?1) * ?1 AS b,
			avg(bat_soc), avg(bat_current), avg(bat_voltage),
			avg(solar_power), avg(grid_power), avg(load_power),
			min(cell_min_mv), max(cell_max_mv)
		FROM %s WHERE device_id = ?2 AND ts >= ?3 AND ts < ?4
		GROUP BY b ORDER BY b`, src.table)
	rows, err := s.db.QueryContext(ctx, q, bucket, deviceID, from, to)
	if err != nil {
		return nil, fmt.Errorf("query history: %w", err)
	}
	defer rows.Close()

	out := newSeries()
	var prev int64 = -1
	for rows.Next() {
		var b int64
		var v [8]sql.NullFloat64
		if err := rows.Scan(&b, &v[0], &v[1], &v[2], &v[3], &v[4], &v[5], &v[6], &v[7]); err != nil {
			return nil, fmt.Errorf("scan history: %w", err)
		}
		if prev >= 0 && b-prev > max(bucket, src.gap) {
			out.add(prev+bucket, [8]sql.NullFloat64{}) // one null breaks the line
		}
		out.add(b, v)
		prev = b
	}
	return out, rows.Err()
}

// bucketSize picks the smallest whole-second bucket that fits span seconds into maxPoints buckets.
func bucketSize(span int64, maxPoints int) int64 {
	n := int64(maxPoints - 1) // alignment can add one partial bucket
	return max(1, (span+n-1)/n)
}

func newSeries() *Series {
	return &Series{TS: []int64{}, BatSoC: []*float64{}, BatCurrent: []*float64{}, BatVoltage: []*float64{},
		SolarPower: []*float64{}, GridPower: []*float64{}, LoadPower: []*float64{},
		CellMinMV: []*float64{}, CellMaxMV: []*float64{}}
}

func (s *Series) add(ts int64, v [8]sql.NullFloat64) {
	s.TS = append(s.TS, ts)
	s.BatSoC = append(s.BatSoC, round(v[0], 1))
	s.BatCurrent = append(s.BatCurrent, round(v[1], 2))
	s.BatVoltage = append(s.BatVoltage, round(v[2], 2))
	s.SolarPower = append(s.SolarPower, round(v[3], 1))
	s.GridPower = append(s.GridPower, round(v[4], 1))
	s.LoadPower = append(s.LoadPower, round(v[5], 1))
	s.CellMinMV = append(s.CellMinMV, round(v[6], 0))
	s.CellMaxMV = append(s.CellMaxMV, round(v[7], 0))
}

func round(v sql.NullFloat64, places int) *float64 {
	if !v.Valid {
		return nil
	}
	p := math.Pow(10, float64(places))
	r := math.Round(v.Float64*p) / p
	return &r
}
