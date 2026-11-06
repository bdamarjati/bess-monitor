package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"bess-monitor/internal/ingest"
	"bess-monitor/internal/store"
)

type fakeStore struct {
	devices []store.Device
	gotID   string
}

func (f *fakeStore) Devices(context.Context) ([]store.Device, error) { return f.devices, nil }

func (f *fakeStore) Latest(_ context.Context, id string) (ingest.Reading, error) {
	f.gotID = id
	if id == "missing" {
		return ingest.Reading{}, store.ErrNotFound
	}
	return ingest.Reading{DeviceID: id, TS: 123}, nil
}

func (f *fakeStore) History(_ context.Context, id string, _, _ int64, _ int) (*store.Series, error) {
	f.gotID = id
	return &store.Series{TS: []int64{}}, nil
}

func get(t *testing.T, h http.Handler, url string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	return rec.Code, rec.Body.String()
}

func TestDeviceResolution(t *testing.T) {
	one := &fakeStore{devices: []store.Device{{ID: "solo"}}}
	if code, body := get(t, newTest(one), "/api/latest"); code != 200 || one.gotID != "solo" || !strings.Contains(body, `"ts":123`) {
		t.Errorf("single device: code=%d id=%q body=%s", code, one.gotID, body)
	}

	two := &fakeStore{devices: []store.Device{{ID: "a"}, {ID: "b"}}}
	if code, _ := get(t, newTest(two), "/api/latest"); code != 400 {
		t.Errorf("two devices without id: code=%d, want 400", code)
	}
	if code, _ := get(t, newTest(two), "/api/latest?device_id=b"); code != 200 || two.gotID != "b" {
		t.Errorf("explicit id: code=%d id=%q", code, two.gotID)
	}

	none := &fakeStore{}
	if code, _ := get(t, newTest(none), "/api/history"); code != 404 {
		t.Errorf("no devices: code=%d, want 404", code)
	}
	if code, _ := get(t, newTest(one), "/api/latest?device_id=missing"); code != 404 {
		t.Errorf("unknown device: code=%d, want 404", code)
	}
}

func TestHistoryHoursValidation(t *testing.T) {
	h := newTest(&fakeStore{devices: []store.Device{{ID: "a"}}})
	for _, q := range []string{"0", "43801", "abc", "1.5", "-1"} {
		if code, _ := get(t, h, "/api/history?hours="+q); code != 400 {
			t.Errorf("hours=%s: code=%d, want 400", q, code)
		}
	}
	for _, q := range []string{"1", "24", "168", "2160", "8760", "43800"} {
		if code, _ := get(t, h, "/api/history?hours="+q); code != 200 {
			t.Errorf("hours=%s: code=%d, want 200", q, code)
		}
	}
}

func TestHealthzAndMethods(t *testing.T) {
	h := newTest(&fakeStore{})
	if code, body := get(t, h, "/healthz"); code != 200 || body != "ok" {
		t.Errorf("healthz: %d %q", code, body)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/devices", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST devices: code=%d, want 405", rec.Code)
	}
}

var testFiles = fstest.MapFS{
	"index.html":   {Data: []byte("<!doctype html>dashboard")},
	"uplot.min.js": {Data: []byte("var uPlot")},
	"login.html":   {Data: []byte("<!doctype html>sign in")},
}

func newTest(st Store) http.Handler { return New(st, testFiles, nil) }

func TestStaticFiles(t *testing.T) {
	h := newTest(&fakeStore{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "dashboard") || rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index: code=%d cache=%q body=%q", rec.Code, rec.Header().Get("Cache-Control"), rec.Body.String())
	}
	if code, _ := get(t, h, "/uplot.min.js?v=1"); code != 200 {
		t.Errorf("asset: code=%d", code)
	}
	if code, body := get(t, h, "/login"); code != 200 || !strings.Contains(body, "sign in") {
		t.Errorf("login page: code=%d body=%q", code, body)
	}
	if code, _ := get(t, h, "/nope.js"); code != 404 {
		t.Errorf("missing file: code=%d, want 404", code)
	}
	if code, _ := get(t, h, "/api/unknown"); code != 404 {
		t.Errorf("unknown api path: code=%d, want 404", code)
	}
}
