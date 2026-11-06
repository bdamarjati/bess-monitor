// Package store handles SQLite access.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"

	"bess-monitor/internal/ingest"
)

const baseSchema = `
CREATE TABLE IF NOT EXISTS readings (
	id            INTEGER PRIMARY KEY,
	device_id     TEXT NOT NULL,
	ts            INTEGER NOT NULL,
	status        INTEGER,
	error_code    INTEGER,
	bat_soc       REAL,
	bat_soh       REAL,
	bat_voltage   REAL,
	bat_current   REAL,
	bat_temps     TEXT,
	bat_cells     TEXT,
	cell_min_mv   INTEGER,
	cell_max_mv   INTEGER,
	solar_voltage REAL,
	solar_current REAL,
	solar_power   REAL,
	grid_voltage  REAL,
	grid_current  REAL,
	grid_power    REAL,
	grid_freq     REAL,
	load_voltage  REAL,
	load_current  REAL,
	load_power    REAL,
	load_freq     REAL
);
CREATE INDEX IF NOT EXISTS readings_device_ts ON readings(device_id, ts);

`

// rollupTable is the schema shared by readings_1m and readings_1h.
// ts is the start of the bucket; the primary key is the unique (device_id, ts) index.
const rollupTable = `
CREATE TABLE IF NOT EXISTS %s (
	device_id     TEXT NOT NULL,
	ts            INTEGER NOT NULL,
	status        INTEGER,
	error_code    INTEGER,
	bat_soc       REAL,
	bat_soh       REAL,
	bat_voltage   REAL,
	bat_current   REAL,
	cell_min_mv   INTEGER,
	cell_max_mv   INTEGER,
	solar_voltage REAL,
	solar_current REAL,
	solar_power   REAL,
	grid_voltage  REAL,
	grid_current  REAL,
	grid_power    REAL,
	grid_freq     REAL,
	load_voltage  REAL,
	load_current  REAL,
	load_power    REAL,
	load_freq     REAL,
	PRIMARY KEY (device_id, ts)
);`

var schema = baseSchema + fmt.Sprintf(rollupTable, "readings_1m") + fmt.Sprintf(rollupTable, "readings_1h")

const insertSQL = `INSERT INTO readings (
	device_id, ts, status, error_code,
	bat_soc, bat_soh, bat_voltage, bat_current, bat_temps, bat_cells, cell_min_mv, cell_max_mv,
	solar_voltage, solar_current, solar_power,
	grid_voltage, grid_current, grid_power, grid_freq,
	load_voltage, load_current, load_power, load_freq
) VALUES (?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?, ?,?,?,?, ?,?,?,?)`

type Store struct {
	db *sql.DB
}

// Open opens (or creates) the database at path and applies the schema.
func Open(path string) (*Store, error) {
	// Pragmas in the DSN apply to every pooled connection.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Insert writes all readings in one transaction.
func (s *Store) Insert(ctx context.Context, rs []ingest.Reading) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() // no-op after commit

	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	for _, r := range rs {
		if _, err := stmt.ExecContext(ctx, args(r)...); err != nil {
			return fmt.Errorf("insert %s: %w", r.DeviceID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// args maps a reading to insert parameters. Nil pointers become NULL.
func args(r ingest.Reading) []any {
	b := r.Battery
	var cellMin, cellMax any
	if lo, hi, ok := b.CellMinMax(); ok {
		cellMin, cellMax = lo, hi
	}
	return []any{
		r.DeviceID, r.TS, r.Status, b.ErrorCode,
		b.SoC, b.SoH, b.Voltage, b.Current, jsonArray(b.Temperature), jsonArray(b.CellVoltage), cellMin, cellMax,
		r.Solar.Voltage, r.Solar.Current, r.Solar.Power,
		r.Grid.Voltage, r.Grid.Current, r.Grid.Power, r.Grid.Freq,
		r.Load.Voltage, r.Load.Current, r.Load.Power, r.Load.Freq,
	}
}

// jsonArray encodes a slice as JSON text, or NULL when absent.
func jsonArray(v []float64) any {
	if v == nil {
		return nil
	}
	b, _ := json.Marshal(v) // []float64 of finite values cannot fail
	return string(b)
}
