package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"bess-monitor/internal/ingest"
)

func TestInsertRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	full := `{"device_id":"d1","status":255,"battery":{"error_code":7,"soc":88,"current":-1.5,
		"temperature":[28.1,28.2],"cell_voltage":[3210,3205,3215]},
		"solar":{"power":100},"grid":{"power":50,"freq":50.1},"load":{"power":150}}`
	sparse := `{"device_id":"d2","status":0,"battery":{},"solar":{},"grid":{},"load":{}}`

	var rs []ingest.Reading
	for _, msg := range []string{full, sparse} {
		r, err := ingest.Decode([]byte(msg), time.Unix(1000, 0))
		if err != nil {
			t.Fatal(err)
		}
		rs = append(rs, r)
	}
	if err := s.Insert(context.Background(), rs); err != nil {
		t.Fatal(err)
	}

	var (
		ts, status         int64
		errCode, lo, hi    sql.NullInt64
		soc, gridFreq, soh sql.NullFloat64
		temps, cells       sql.NullString
	)
	q := `SELECT ts, status, error_code, bat_soc, bat_soh, grid_freq, bat_temps, bat_cells, cell_min_mv, cell_max_mv
	      FROM readings WHERE device_id = ?`

	if err := s.db.QueryRow(q, "d1").Scan(&ts, &status, &errCode, &soc, &soh, &gridFreq, &temps, &cells, &lo, &hi); err != nil {
		t.Fatal(err)
	}
	if ts != 1000 || status != 255 || errCode.Int64 != 7 || soc.Float64 != 88 || soh.Valid || gridFreq.Float64 != 50.1 {
		t.Errorf("d1 scalars wrong: ts=%d status=%d err=%v soc=%v soh=%v freq=%v", ts, status, errCode, soc, soh, gridFreq)
	}
	if temps.String != "[28.1,28.2]" || cells.String != "[3210,3205,3215]" || lo.Int64 != 3205 || hi.Int64 != 3215 {
		t.Errorf("d1 arrays wrong: temps=%v cells=%v min=%v max=%v", temps, cells, lo, hi)
	}

	if err := s.db.QueryRow(q, "d2").Scan(&ts, &status, &errCode, &soc, &soh, &gridFreq, &temps, &cells, &lo, &hi); err != nil {
		t.Fatal(err)
	}
	if errCode.Valid || soc.Valid || temps.Valid || cells.Valid || lo.Valid || hi.Valid {
		t.Errorf("d2 missing fields should be NULL: err=%v soc=%v temps=%v cells=%v min=%v", errCode, soc, temps, cells, lo)
	}
}

func TestPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var mode string
	var sync int
	s.db.QueryRow("PRAGMA journal_mode").Scan(&mode)
	s.db.QueryRow("PRAGMA synchronous").Scan(&sync)
	if mode != "wal" || sync != 1 { // 1 = NORMAL
		t.Errorf("journal_mode=%q synchronous=%d", mode, sync)
	}
}
