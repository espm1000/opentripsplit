package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"golang.org/x/oauth2"

	"github.com/espm1000/opentripsplit/internal/store"
)

const (
	sessionCookie = "ots_session"
	flowCookie    = "ots_oauth"
	flowTTL       = 10 * time.Minute
)

// Auth handles login, logout, account linking and sessions.
type Auth struct {
	Store      *store.Store
	SessionTTL time.Duration
	// Secure sets the Secure flag on cookies. Enable it when BASE_URL is
	// https; Safari drops Secure cookies on plain http://localhost.
	Secure bool
	Log    *slog.Logger
	// Fail renders an error page. Defaults to http.Error.
	Fail func(w http.ResponseWriter, r *http.Request, status int, msg string)
	// OnLogin, if set, runs after a successful login (not after linking).
	OnLogin func(w http.ResponseWriter, r *http.Request, u store.User)

	providers atomic.Pointer[[]*Provider]
}

// Providers returns the enabled login providers.
func (a *Auth) Providers() []*Provider {
	if p := a.providers.Load(); p != nil {
		return *p
	}
	return nil
}

// SetProviders replaces the enabled providers. Safe to call while serving,
// e.g. after an admin changes credentials in the UI.
func (a *Auth) SetProviders(ps []*Provider) { a.providers.Store(&ps) }

// Provider returns the enabled provider with the given name.
func (a *Auth) Provider(name string) *Provider {
	for _, p := range a.Providers() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func (a *Auth) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if a.Fail != nil {
		a.Fail(w, r, status, msg)
		return
	}
	http.Error(w, msg, status)
}

type ctxKey struct{}

// UserFrom returns the logged-in user, if any.
func UserFrom(ctx context.Context) (store.User, bool) {
	u, ok := ctx.Value(ctxKey{}).(store.User)
	return u, ok
}

// WithUser returns a context carrying u. Exposed for tests.
func WithUser(ctx context.Context, u store.User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// HashToken returns the SHA-256 of a cookie or link token. Only the hash is
// stored, so a database leak doesn't leak usable tokens.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// NewToken returns 32 random bytes, base64url-encoded.
func NewToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return base64.RawURLEncoding.EncodeToString(b)
}

// LoadUser puts the session's user (if any) in the request context.
func (a *Auth) LoadUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
			u, err := a.Store.SessionUser(r.Context(), HashToken(c.Value))
			switch {
			case err == nil:
				r = r.WithContext(WithUser(r.Context(), u))
			case !errors.Is(err, store.ErrNotFound):
				a.Log.Error("load session", "err", err)
			}
		}
		next.ServeHTTP(w, r)
	})
}

// StartSession creates a session for userID, replacing any session cookie
// the request already carried, and sets the cookie.
func (a *Auth) StartSession(w http.ResponseWriter, r *http.Request, userID int64) error {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := a.Store.DeleteSession(r.Context(), HashToken(c.Value)); err != nil {
			return err
		}
	}
	token := NewToken()
	expires := time.Now().Add(a.SessionTTL)
	if err := a.Store.CreateSession(r.Context(), HashToken(token), userID, expires); err != nil {
		return err
	}
	http.SetCookie(w, a.Cookie(sessionCookie, token, "/", expires))
	return nil
}

// Cookie builds an HttpOnly, SameSite=Lax cookie, Secure when configured.
// A zero expires deletes the cookie.
func (a *Auth) Cookie(name, value, path string, expires time.Time) *http.Cookie {
	c := &http.Cookie{ // #nosec G124 -- Secure follows BASE_URL (https); HttpOnly and SameSite=Lax are always set
		Name: name, Value: value, Path: path,
		HttpOnly: true, Secure: a.Secure, SameSite: http.SameSiteLaxMode,
	}
	if expires.IsZero() {
		c.MaxAge = -1
	} else {
		c.Expires = expires
	}
	return c
}

// Logout deletes the session.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := a.Store.DeleteSession(r.Context(), HashToken(c.Value)); err != nil {
			a.Log.Error("logout", "err", err)
		}
	}
	a.ClearSession(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// ClearSession expires the session cookie.
func (a *Auth) ClearSession(w http.ResponseWriter) {
	http.SetCookie(w, a.Cookie(sessionCookie, "", "/", time.Time{}))
}

// flow is the login state kept in a short-lived cookie between the redirect
// to the provider and the callback. It must be SameSite=Lax (not Strict) or
// the browser won't send it on the provider's redirect back.
type flow struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Provider string `json:"p"`
	Next     string `json:"n,omitempty"`
	Link     bool   `json:"l,omitempty"`
}

// SafeNext returns next if it's a same-site path, otherwise "". Control
// characters are refused: browsers strip tabs and newlines from URLs, so
// "/\t/evil.example" would become "//evil.example". Backslashes are refused
// anywhere, not just at the start: browsers read "\" as "/", and
// http.Redirect cleans dot segments after this check, turning
// "/./\evil.example" into "/\evil.example". No app path contains one.
func SafeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') ||
		strings.ContainsFunc(next, unicode.IsControl) {
		return ""
	}
	return next
}

// Login redirects to the provider. GET /auth/{provider}/login[?next=/path][&link=1]
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	p := a.Provider(r.PathValue("provider"))
	if p == nil {
		a.fail(w, r, http.StatusNotFound, "Unknown login provider.")
		return
	}
	f := flow{
		State:    NewToken(),
		Verifier: oauth2.GenerateVerifier(),
		Provider: p.Name,
		Next:     SafeNext(r.URL.Query().Get("next")),
		Link:     r.URL.Query().Get("link") == "1",
	}
	if _, ok := UserFrom(r.Context()); f.Link && !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	b, _ := json.Marshal(f)
	http.SetCookie(w, a.Cookie(flowCookie, base64.RawURLEncoding.EncodeToString(b), "/auth/", time.Now().Add(flowTTL)))
	http.Redirect(w, r, p.Config.AuthCodeURL(f.State, oauth2.S256ChallengeOption(f.Verifier)), http.StatusFound)
}

// Callback finishes the OAuth flow: GET /auth/{provider}/callback
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	p := a.Provider(r.PathValue("provider"))
	if p == nil {
		a.fail(w, r, http.StatusNotFound, "Unknown login provider.")
		return
	}
	var f flow
	c, err := r.Cookie(flowCookie)
	if err == nil {
		var b []byte
		if b, err = base64.RawURLEncoding.DecodeString(c.Value); err == nil {
			err = json.Unmarshal(b, &f)
		}
	}
	http.SetCookie(w, a.Cookie(flowCookie, "", "/auth/", time.Time{}))
	q := r.URL.Query()
	if err != nil || f.Provider != p.Name || f.State == "" ||
		subtle.ConstantTimeCompare([]byte(f.State), []byte(q.Get("state"))) != 1 {
		// err is set when the flow cookie is missing or unreadable.
		a.Log.Warn("oauth callback: login state missing or mismatched", "provider", p.Name, "err", err)
		a.fail(w, r, http.StatusBadRequest, "Login expired or was started in another tab. Please try again.")
		return
	}
	if q.Get("error") != "" || q.Get("code") == "" {
		// Never log code or state. error shows misconfiguration such as
		// redirect_uri_mismatch, or the user pressing cancel (access_denied).
		a.Log.Warn("oauth callback: provider returned no code", "provider", p.Name,
			"error", q.Get("error"), "error_description", q.Get("error_description"))
		a.fail(w, r, http.StatusBadRequest, "Login was cancelled.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: 15 * time.Second})
	pu, err := p.fetchUser(ctx, q.Get("code"), f.Verifier)
	if err != nil {
		a.Log.Error("oauth callback", "provider", p.Name, "err", err)
		a.fail(w, r, http.StatusBadGateway, "Couldn't complete login with "+p.Label+". Please try again.")
		return
	}

	if f.Link {
		u, ok := UserFrom(r.Context())
		if !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		err := a.Store.LinkIdentity(r.Context(), u.ID, p.Name, pu.ID)
		if errors.Is(err, store.ErrIdentityTaken) {
			a.fail(w, r, http.StatusConflict, "That "+p.Label+" account is already linked to a different OpenTripSplit account.")
			return
		}
		if err != nil {
			a.Log.Error("link identity", "err", err)
			a.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
			return
		}
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}

	u, err := a.Store.LoginIdentity(r.Context(), p.Name, pu.ID, pu.Name)
	if err == nil {
		err = a.StartSession(w, r, u.ID)
	}
	if err != nil {
		a.Log.Error("login", "err", err)
		a.fail(w, r, http.StatusInternalServerError, "Something went wrong.")
		return
	}
	if a.OnLogin != nil {
		a.OnLogin(w, r, u)
	}
	// Login checked Next, but the flow cookie is unsigned: check it again.
	next := SafeNext(f.Next)
	if next == "" {
		next = "/groups"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
