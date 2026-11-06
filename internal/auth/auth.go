// Package auth protects the dashboard with a username/password login and a
// signed session cookie. Standard library only.
package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	cookieName = "bess_session"
	sessionTTL = 7 * 24 * time.Hour
)

type Auth struct {
	userHash, passHash [32]byte
	user               string
	key                []byte // HMAC key for session cookies
	limiter            *limiter
	now                func() time.Time
}

// New creates an Auth for one user. secret signs session cookies; if empty a
// random one is generated, so sessions end when the app restarts.
// The key also depends on the password, so changing it logs everyone out.
func New(user, pass, secret string) (*Auth, error) {
	if user == "" || pass == "" {
		return nil, errors.New("DASH_USER and DASH_PASS must be set")
	}
	s := []byte(secret)
	if len(s) == 0 {
		s = make([]byte, 32)
		if _, err := rand.Read(s); err != nil {
			return nil, err
		}
		slog.Info("SESSION_SECRET not set; sessions will end when the app restarts")
	}
	mac := hmac.New(sha256.New, s)
	mac.Write([]byte(pass))
	return &Auth{
		userHash: sha256.Sum256([]byte(user)),
		passHash: sha256.Sum256([]byte(pass)),
		user:     user,
		key:      mac.Sum(nil),
		limiter:  newLimiter(),
		now:      time.Now,
	}, nil
}

// Middleware lets GET /login and /healthz through and requires a valid session
// for everything else: API calls get 401, pages are redirected to /login.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/login" || a.valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"not signed in"}` + "\n"))
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// LoginPage wraps the GET /login page handler: signed-in users go straight to the dashboard.
func (a *Auth) LoginPage(page http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.valid(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		page.ServeHTTP(w, r)
	})
}

// Login handles the sign-in form (POST /login, fields "user" and "pass").
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if a.limiter.blocked(ip, a.now()) {
		http.Redirect(w, r, "/login?error=locked", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	u := sha256.Sum256([]byte(r.PostForm.Get("user")))
	p := sha256.Sum256([]byte(r.PostForm.Get("pass")))
	okUser := subtle.ConstantTimeCompare(u[:], a.userHash[:])
	okPass := subtle.ConstantTimeCompare(p[:], a.passHash[:])
	if okUser&okPass != 1 {
		a.limiter.fail(ip, a.now())
		slog.Warn("sign-in failed", "ip", ip)
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	a.limiter.reset(ip)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    a.sign(a.now().Add(sessionTTL)),
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout clears the session cookie (POST /logout).
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Cookie format: base64(payload) "." base64(hmac(payload)), payload = {"u":user,"e":expiry}.
type payload struct {
	User   string `json:"u"`
	Expiry int64  `json:"e"`
}

func (a *Auth) sign(exp time.Time) string {
	b, _ := json.Marshal(payload{User: a.user, Expiry: exp.Unix()})
	return enc(b) + "." + enc(a.mac(b))
}

func (a *Auth) valid(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return false
	}
	b, err1 := base64.RawURLEncoding.DecodeString(body)
	s, err2 := base64.RawURLEncoding.DecodeString(sig)
	if err1 != nil || err2 != nil || !hmac.Equal(s, a.mac(b)) {
		return false
	}
	var p payload
	if json.Unmarshal(b, &p) != nil {
		return false
	}
	return p.User == a.user && a.now().Unix() < p.Expiry
}

func (a *Auth) mac(b []byte) []byte {
	m := hmac.New(sha256.New, a.key)
	m.Write(b)
	return m.Sum(nil)
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// isHTTPS reports whether the browser used HTTPS, directly or via Caddy.
func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

// clientIP returns the caller's IP. Behind a local reverse proxy (Caddy) it uses
// the last X-Forwarded-For entry, which the proxy itself appends; earlier entries
// can be forged by the client.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}
