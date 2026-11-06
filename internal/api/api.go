// Package api serves the JSON API and the embedded dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"bess-monitor/internal/ingest"
	"bess-monitor/internal/store"
)

const (
	maxPoints  = 500
	defaultHrs = 24
	maxHours   = 5 * 365 * 24 // 43800
	reqTimeout = 10 * time.Second
)

// Store is the subset of store.Store used by the handlers.
type Store interface {
	Devices(ctx context.Context) ([]store.Device, error)
	Latest(ctx context.Context, deviceID string) (ingest.Reading, error)
	History(ctx context.Context, deviceID string, from, to int64, maxPoints int) (*store.Series, error)
}

type server struct {
	st  Store
	now func() time.Time
}

// New returns the HTTP handler for the API, health check, and the static
// dashboard files in static. loginPage wraps the GET /login page (e.g. to send
// signed-in users to the dashboard); nil serves it as-is.
func New(st Store, static fs.FS, loginPage func(http.Handler) http.Handler) http.Handler {
	s := &server{st: st, now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/devices", s.devices)
	mux.HandleFunc("GET /api/latest", s.latest)
	mux.HandleFunc("GET /api/history", s.history)
	mux.Handle("GET /", staticFiles(static))

	page := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, static, "login.html")
	}))
	if loginPage != nil {
		page = loginPage(page)
	}
	mux.Handle("GET /login", page)
	return mux
}

// staticFiles serves the dashboard. index.html must always be revalidated;
// long-term caching of the versioned assets is left to Caddy.
func staticFiles(static fs.FS) http.Handler {
	files := http.FileServerFS(static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func (s *server) devices(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), reqTimeout)
	defer cancel()
	ds, err := s.st.Devices(ctx)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, ds)
}

func (s *server) latest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), reqTimeout)
	defer cancel()
	id, ok := s.deviceID(ctx, w, r)
	if !ok {
		return
	}
	rd, err := s.st.Latest(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		httpError(w, http.StatusNotFound, "no data for device")
		return
	}
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, rd)
}

func (s *server) history(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), reqTimeout)
	defer cancel()

	hours := defaultHrs
	if v := r.URL.Query().Get("hours"); v != "" {
		h, err := strconv.Atoi(v)
		if err != nil || h < 1 || h > maxHours {
			httpError(w, http.StatusBadRequest, "hours must be an integer from 1 to 43800")
			return
		}
		hours = h
	}
	id, ok := s.deviceID(ctx, w, r)
	if !ok {
		return
	}

	to := s.now().Unix() + 1 // include readings stamped this second
	from := to - int64(hours)*3600
	series, err := s.st.History(ctx, id, from, to, maxPoints)
	if err != nil {
		serverError(w, err)
		return
	}
	writeJSON(w, series)
}

// deviceID returns the device_id query param, or the only known device when omitted.
// On failure it writes the error response and returns ok=false.
func (s *server) deviceID(ctx context.Context, w http.ResponseWriter, r *http.Request) (string, bool) {
	if id := r.URL.Query().Get("device_id"); id != "" {
		return id, true
	}
	ds, err := s.st.Devices(ctx)
	if err != nil {
		serverError(w, err)
		return "", false
	}
	switch len(ds) {
	case 1:
		return ds[0].ID, true
	case 0:
		httpError(w, http.StatusNotFound, "no devices yet")
	default:
		httpError(w, http.StatusBadRequest, "device_id is required when more than one device exists")
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err)
	}
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func serverError(w http.ResponseWriter, err error) {
	slog.Error("request failed", "err", err)
	httpError(w, http.StatusInternalServerError, "internal error")
}
