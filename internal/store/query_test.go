package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"bess-monitor/internal/ingest"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func reading(t *testing.T, device string, ts int64, soc float64, cells string) ingest.Reading {
	t.Helper()
	msg := fmt.Sprintf(`{"device_id":%q,"status":0,"battery":{"soc":%v,"current":-1.234,"temperature":[25],"cell_voltage":%s},
		"solar":{"power":100.04},"grid":{"power":0},"load":{"power":100,"freq":50.0}}`, device, soc, cells)
	r, err := ingest.Decode([]byte(msg), time.Unix(ts, 0))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBucketSizeFitsMaxPoints(t *testing.T) {
	for _, hours := range []int64{1, 6, 24, 168} {
		rng := hours * 3600
		b := bucketSize(rng, 500)
		// worst case: range not aligned to bucket boundaries
		if n := rng/b + 1; n > 500 {
			t.Errorf("%dh: bucket %ds gives up to %d points", hours, b, n)
		}
	}
	if b := bucketSize(10, 500); b != 1 {
		t.Errorf("tiny range bucket = %d, want 1", b)
	}
}

func TestHistoryDownsamplesAndMarksGaps(t *testing.T) {
	s := openTest(t)
	const start = 1_700_000_000
	var rs []ingest.Reading
	for ts := int64(start); ts < start+2*3600; ts += 5 {
		if ts >= start+3600 && ts < start+3600+600 {
			continue // 10-minute outage
		}
		rs = append(rs, reading(t, "d1", ts, 50, "[3200,3190,3210]"))
	}
	rs = append(rs, reading(t, "other", start+10, 99, "[1,2]")) // must not leak into d1
	if err := s.Insert(context.Background(), rs); err != nil {
		t.Fatal(err)
	}

	got, err := s.History(context.Background(), "d1", start, start+2*3600, 500)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(got.TS); n == 0 || n > 500 {
		t.Fatalf("got %d points, want 1..500", n)
	}
	if len(got.BatSoC) != len(got.TS) || len(got.CellMaxMV) != len(got.TS) {
		t.Fatal("columns have different lengths")
	}

	gaps := 0
	for i := range got.TS {
		if got.BatSoC[i] == nil {
			gaps++
			continue
		}
		if *got.BatSoC[i] != 50 || *got.CellMinMV[i] != 3190 || *got.CellMaxMV[i] != 3210 {
			t.Fatalf("point %d: soc=%v min=%v max=%v", i, *got.BatSoC[i], *got.CellMinMV[i], *got.CellMaxMV[i])
		}
		if *got.BatCurrent[i] != -1.23 || *got.SolarPower[i] != 100 {
			t.Fatalf("point %d not rounded: current=%v solar=%v", i, *got.BatCurrent[i], *got.SolarPower[i])
		}
		if i > 0 && got.TS[i] <= got.TS[i-1] {
			t.Fatal("timestamps not increasing")
		}
	}
	if gaps != 1 {
		t.Errorf("got %d gap markers, want 1", gaps)
	}
}

func TestHistorySourceByRange(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const now = 1_700_000_000
	s.db.Exec(`INSERT INTO readings_1m (device_id, ts, bat_soc) VALUES ('d1', ?, 42)`, now-2*86400)
	s.db.Exec(`INSERT INTO readings_1h (device_id, ts, bat_soc) VALUES ('d1', ?, 77)`, now-100*86400)
	if err := s.Insert(ctx, []ingest.Reading{reading(t, "d1", now-60, 10, "[1]")}); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		hours int64
		want  float64
	}{{24, 10}, {7 * 24, 42}, {90 * 24, 42}, {365 * 24, 77}}
	for _, c := range cases {
		got, err := s.History(ctx, "d1", now-c.hours*3600, now, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(got.TS) != 1 || *got.BatSoC[0] != c.want {
			t.Errorf("%dh: got %v, want one point = %v", c.hours, got.BatSoC, c.want)
		}
	}
}

func TestHistoryReturnsSmallResultsAsIs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	const now = 1_800_000_000 / 3600 * 3600
	// 10 days of hourly rows = 240 rows, requested over 1 year.
	for i := int64(1); i <= 240; i++ {
		s.db.Exec(`INSERT INTO readings_1h (device_id, ts, bat_soc) VALUES ('d1', ?, ?)`, now-i*3600, i)
	}
	got, _ := s.History(ctx, "d1", now-365*86400, now, 500)
	if len(got.TS) != 240 || got.TS[0] != now-240*3600 || *got.BatSoC[0] != 240 {
		t.Fatalf("got %d points (first ts %d), want the 240 rows unchanged", len(got.TS), got.TS[0])
	}
	for _, v := range got.BatSoC {
		if v == nil {
			t.Fatal("hourly rows must not be marked as gaps")
		}
	}

	// 2 years of hourly rows over 5 years: downsampled to <= 500 points.
	for i := int64(241); i <= 2*8760; i++ {
		s.db.Exec(`INSERT INTO readings_1h (device_id, ts, bat_soc) VALUES ('d1', ?, 1)`, now-i*3600)
	}
	got, _ = s.History(ctx, "d1", now-5*365*86400, now, 500)
	if n := len(got.TS); n < 400 || n > 500 {
		t.Errorf("downsampled to %d points, want 400..500", n)
	}
}

func TestLatestAndDevices(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.Latest(ctx, "d1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty db: err=%v, want ErrNotFound", err)
	}
	s.Insert(ctx, []ingest.Reading{
		reading(t, "d1", 100, 10, "[3200,3201]"),
		reading(t, "d1", 200, 20, "[3300,3301]"),
		reading(t, "d2", 150, 30, "[1]"),
	})

	r, err := s.Latest(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if r.TS != 200 || *r.Battery.SoC != 20 || len(r.Battery.CellVoltage) != 2 || r.Battery.SoH != nil {
		t.Errorf("latest = %+v", r)
	}

	ds, _ := s.Devices(ctx)
	if len(ds) != 2 || ds[0] != (Device{"d1", 200}) || ds[1] != (Device{"d2", 150}) {
		t.Errorf("devices = %+v", ds)
	}
}
