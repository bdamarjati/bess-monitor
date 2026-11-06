package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"bess-monitor/internal/ingest"
)

const (
	hour = int64(3600)
	day  = 24 * hour
	h0   = int64(1_700_002_800) // a whole hour
)

func faultReading(t *testing.T, device string, ts int64, status, errCode int, soc float64, cells string) ingest.Reading {
	t.Helper()
	msg := fmt.Sprintf(`{"device_id":%q,"status":%d,"battery":{"error_code":%d,"soc":%v,"cell_voltage":%s},
		"solar":{},"grid":{},"load":{"power":100}}`, device, status, errCode, soc, cells)
	r, err := ingest.Decode([]byte(msg), time.Unix(ts, 0))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// ok inserts a normal reading.
func ok(t *testing.T, device string, ts int64, soc float64) ingest.Reading {
	return faultReading(t, device, ts, 0, 0, soc, "[3200,3210]")
}

func insert(t *testing.T, s *Store, rs ...ingest.Reading) {
	t.Helper()
	if err := s.Insert(context.Background(), rs); err != nil {
		t.Fatal(err)
	}
}

type bucketRow struct {
	status, errCode, cellMin, cellMax int64
	soc                               float64
}

func getBucket(t *testing.T, s *Store, table, device string, ts int64) (bucketRow, bool) {
	t.Helper()
	var r bucketRow
	err := s.db.QueryRow(fmt.Sprintf(`SELECT status, error_code, bat_soc, cell_min_mv, cell_max_mv
		FROM %s WHERE device_id = ? AND ts = ?`, table), device, ts).
		Scan(&r.status, &r.errCode, &r.soc, &r.cellMin, &r.cellMax)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return r, true
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// rollupAll runs both rollups as the maintenance loop does.
func rollupAll(t *testing.T, s *Store, now int64) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.RollupMinutes(ctx, time.Unix(now, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RollupHours(ctx, time.Unix(now, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestFaultSurvivesMinuteAndHourRollup(t *testing.T) {
	s := openTest(t)
	m := h0 + 10*60 // 10 minutes into the hour
	insert(t, s,
		ok(t, "d1", h0, 40),
		ok(t, "d1", m, 50),
		faultReading(t, "d1", m+20, 255, 7, 52, "[3150,3220]"), // the only fault
		faultReading(t, "d1", m+40, 0, 3, 54, "[3205,3215]"),
		ok(t, "d1", h0+30*60, 60),
		ok(t, "d2", m, 10), // other device
	)
	rollupAll(t, s, h0+hour+60)

	got, _ := getBucket(t, s, "readings_1m", "d1", m)
	if want := (bucketRow{status: 255, errCode: 7, soc: 52, cellMin: 3150, cellMax: 3220}); got != want {
		t.Errorf("minute = %+v, want %+v", got, want)
	}
	got, _ = getBucket(t, s, "readings_1h", "d1", h0)
	if got.status != 255 || got.errCode != 7 || got.cellMin != 3150 || got.cellMax != 3220 {
		t.Errorf("hour lost the fault: %+v", got)
	}
	if got.soc != (40+52+60)/3.0 { // average of the three minute averages
		t.Errorf("hour soc = %v", got.soc)
	}
	if d2, _ := getBucket(t, s, "readings_1h", "d2", h0); d2.status != 0 || d2.soc != 10 {
		t.Errorf("d2 hour = %+v", d2)
	}
}

func TestRollupIsIdempotent(t *testing.T) {
	s := openTest(t)
	for i := int64(0); i < 90; i++ {
		insert(t, s, ok(t, "d1", h0+i*60, float64(i)))
	}
	now := h0 + 2*hour + 60
	rollupAll(t, s, now)
	m1, h1 := count(t, s, "readings_1m"), count(t, s, "readings_1h")
	first, _ := getBucket(t, s, "readings_1h", "d1", h0)

	rollupAll(t, s, now) // same time again
	rollupAll(t, s, now+5*60)
	if m2, h2 := count(t, s, "readings_1m"), count(t, s, "readings_1h"); m1 != 90 || h1 != 2 || m2 != m1 || h2 != h1 {
		t.Errorf("rows: first run %d/%d, re-runs %d/%d, want 90/2 both times", m1, h1, m2, h2)
	}
	if again, _ := getBucket(t, s, "readings_1h", "d1", h0); again != first {
		t.Errorf("re-run changed hour row: %+v -> %+v", first, again)
	}
}

func TestRollupSkipsIncompleteBuckets(t *testing.T) {
	s := openTest(t)
	insert(t, s, ok(t, "d1", h0, 1), ok(t, "d1", h0+65, 2))
	rollupAll(t, s, h0+100) // second minute and the hour are still running

	if _, found := getBucket(t, s, "readings_1m", "d1", h0); !found {
		t.Error("completed minute was not rolled up")
	}
	if _, found := getBucket(t, s, "readings_1m", "d1", h0+60); found {
		t.Error("current minute was rolled up")
	}
	if n := count(t, s, "readings_1h"); n != 0 {
		t.Errorf("current hour was rolled up (%d rows)", n)
	}
}

func TestCatchUpAfterDowntime(t *testing.T) {
	s := openTest(t)
	// One hour of data, rolled up while the app was running.
	for ts := h0; ts < h0+hour; ts += 30 {
		insert(t, s, ok(t, "d1", ts, 50))
	}
	rollupAll(t, s, h0+hour+60)

	// The app is down for 5 hours while readings keep arriving (e.g. batched elsewhere),
	// and a new device appears.
	for ts := h0 + hour; ts < h0+6*hour; ts += 30 {
		insert(t, s, ok(t, "d1", ts, 60), ok(t, "d2", ts, 70))
	}
	rollupAll(t, s, h0+6*hour+60)

	if n := count(t, s, "readings_1m"); n != 60+2*5*60 {
		t.Errorf("minute rows = %d, want %d", n, 60+2*5*60)
	}
	if n := count(t, s, "readings_1h"); n != 6+5 {
		t.Errorf("hour rows = %d, want 11", n)
	}
	if r, _ := getBucket(t, s, "readings_1h", "d2", h0+5*hour); r.soc != 70 {
		t.Errorf("last hour for new device = %+v", r)
	}
}

func TestCleanupNeverDeletesUnrolledData(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	now := time.Unix(h0+400*day, 0)
	n := now.Unix()
	ret := DefaultRetention

	// Raw: two old minutes, only the first rolled up.
	old := (n - 8*day) / 60 * 60
	insert(t, s, ok(t, "d1", old, 1), ok(t, "d1", old+60, 2), ok(t, "d1", n-60, 3))
	s.aggregate(ctx, minuteTier, "d1", old, old+60)

	// Minute rows: two old hours, only the first rolled up.
	oldH := (n - 91*day) / hour * hour
	for _, ts := range []int64{oldH, oldH + hour, n - 5*day} {
		s.db.Exec(`INSERT INTO readings_1m (device_id, ts, bat_soc) VALUES ('d1', ?, 1)`, ts)
	}
	s.aggregate(ctx, hourTier, "d1", oldH, oldH+hour)

	// Hour rows: one past 5 years, one inside.
	s.db.Exec(`INSERT INTO readings_1h (device_id, ts) VALUES ('d1', ?), ('d1', ?)`, n-1826*day, n-1000*day)

	raw, minute, hr, err := s.Cleanup(ctx, now, ret)
	if err != nil {
		t.Fatal(err)
	}
	// minute row at `old` (created above) is not deleted: it is younger than 90 days.
	if raw != 1 || minute != 1 || hr != 1 {
		t.Errorf("deleted raw=%d 1m=%d 1h=%d, want 1 each", raw, minute, hr)
	}
	var left int
	s.db.QueryRow(`SELECT count(*) FROM readings WHERE ts = ?`, old+60).Scan(&left)
	if left != 1 {
		t.Error("un-rolled-up raw row was deleted")
	}
	s.db.QueryRow(`SELECT count(*) FROM readings_1m WHERE ts = ?`, oldH+hour).Scan(&left)
	if left != 1 {
		t.Error("un-rolled-up minute row was deleted")
	}

	// Once rolled up (as the maintenance loop does before cleanup), they go too.
	s.aggregate(ctx, minuteTier, "d1", old+60, old+120)
	s.aggregate(ctx, hourTier, "d1", oldH+hour, oldH+2*hour)
	raw, minute, _, _ = s.Cleanup(ctx, now, ret)
	if raw != 1 || minute != 1 {
		t.Errorf("second cleanup deleted raw=%d 1m=%d, want 1 each", raw, minute)
	}
}

func TestCleanupDeletesWholeBucketsOnly(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	m := h0 + 7*60
	insert(t, s, ok(t, "d1", m+10, 1), ok(t, "d1", m+50, 2))
	s.aggregate(ctx, minuteTier, "d1", m, m+60)

	// Retention cutoff falls in the middle of the minute.
	now := time.Unix(m+30, 0).Add(DefaultRetention.Raw)
	raw, _, _, _ := s.Cleanup(ctx, now, DefaultRetention)
	if raw != 0 || count(t, s, "readings") != 2 {
		t.Errorf("deleted %d rows of a minute the cutoff splits", raw)
	}
}
