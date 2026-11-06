package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func newTest(t *testing.T) (*Auth, http.Handler) {
	t.Helper()
	a, err := New("admin", "s3cret", "test-secret")
	if err != nil {
		t.Fatal(err)
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("protected")) })
	mux := http.NewServeMux()
	mux.Handle("/", a.Middleware(ok))
	mux.HandleFunc("POST /login", a.Login)
	mux.HandleFunc("POST /logout", a.Logout)
	return a, mux
}

func login(h http.Handler, user, pass, ip string) *httptest.ResponseRecorder {
	form := url.Values{"user": {user}, "pass": {pass}}
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = ip + ":1234"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func get(h http.Handler, path string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestRequiresSession(t *testing.T) {
	_, h := newTest(t)
	if rec := get(h, "/"); rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Errorf("page: %d -> %q, want redirect to /login", rec.Code, rec.Header().Get("Location"))
	}
	if rec := get(h, "/api/latest"); rec.Code != http.StatusUnauthorized {
		t.Errorf("api: %d, want 401", rec.Code)
	}
	for _, p := range []string{"/login", "/healthz"} {
		if rec := get(h, p); rec.Code != 200 {
			t.Errorf("%s should be public, got %d", p, rec.Code)
		}
	}
}

func TestLoginAndLogout(t *testing.T) {
	_, h := newTest(t)
	rec := login(h, "admin", "s3cret", "10.0.0.1")
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login: %d -> %q", rec.Code, rec.Header().Get("Location"))
	}
	c := rec.Result().Cookies()[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie flags: %+v", c)
	}
	if rec := get(h, "/api/latest", c); rec.Body.String() != "protected" {
		t.Errorf("with session: %d %q", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if cl := out.Result().Cookies(); len(cl) != 1 || cl[0].MaxAge >= 0 {
		t.Errorf("logout should expire the cookie: %+v", cl)
	}
}

func TestWrongPasswordAndLockout(t *testing.T) {
	_, h := newTest(t)
	for i := 0; i < maxFails; i++ {
		rec := login(h, "admin", "wrong", "10.0.0.2")
		if loc := rec.Header().Get("Location"); loc != "/login?error=1" {
			t.Fatalf("attempt %d: redirect %q", i+1, loc)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Fatal("failed login set a cookie")
		}
	}
	// Locked out now, even with the right password.
	if loc := login(h, "admin", "s3cret", "10.0.0.2").Header().Get("Location"); loc != "/login?error=locked" {
		t.Errorf("after %d failures: %q, want locked", maxFails, loc)
	}
	// Another IP is not affected.
	if loc := login(h, "admin", "s3cret", "10.0.0.3").Header().Get("Location"); loc != "/" {
		t.Errorf("other ip: %q", loc)
	}
}

func TestLockoutExpires(t *testing.T) {
	a, h := newTest(t)
	now := time.Now()
	a.now = func() time.Time { return now }
	for i := 0; i < maxFails; i++ {
		login(h, "admin", "wrong", "10.0.0.4")
	}
	now = now.Add(failWindow + time.Second)
	if loc := login(h, "admin", "s3cret", "10.0.0.4").Header().Get("Location"); loc != "/" {
		t.Errorf("after the window: %q, want success", loc)
	}
}

func TestRejectsBadCookies(t *testing.T) {
	a, h := newTest(t)
	valid := a.sign(time.Now().Add(time.Hour))
	body, sig, _ := strings.Cut(valid, ".")

	other, _ := New("admin", "different-password", "test-secret")
	cases := map[string]string{
		"tampered payload": enc([]byte(`{"u":"admin","e":99999999999}`)) + "." + sig,
		"tampered sig":     body + "." + enc([]byte("nope")),
		"no dot":           body,
		"expired":          a.sign(time.Now().Add(-time.Minute)),
		"old password":     other.sign(time.Now().Add(time.Hour)),
	}
	for name, v := range cases {
		if rec := get(h, "/api/x", &http.Cookie{Name: cookieName, Value: v}); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, rec.Code)
		}
	}
}

func TestClientIPBehindProxy(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.7") // first entry forged by the client
	if ip := clientIP(req); ip != "203.0.113.7" {
		t.Errorf("behind proxy: %q", ip)
	}
	req.RemoteAddr = "198.51.100.1:5000" // not a local proxy: ignore the header
	if ip := clientIP(req); ip != "198.51.100.1" {
		t.Errorf("direct: %q", ip)
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	if _, err := New("", "x", ""); err == nil {
		t.Error("empty user accepted")
	}
	if _, err := New("x", "", ""); err == nil {
		t.Error("empty password accepted")
	}
}
