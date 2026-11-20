package web

import (
	"net/http"
	"slices"
	"time"

	"github.com/espm1000/opentripsplit/internal/auth"
)

// Display preferences (colour theme and text size) are per browser, kept
// in cookies and applied on the server so the page never flashes the wrong
// theme. They work logged out too.

const (
	themeCookie = "ots_theme"
	sizeCookie  = "ots_size"
	prefsTTL    = 365 * 24 * time.Hour
)

var (
	themes    = []string{"system", "dark", "light"}
	textSizes = []string{"normal", "large", "huge"}
)

// pref returns the cookie value if it's one of allowed, else allowed[0].
func pref(r *http.Request, cookie string, allowed []string) string {
	if c, err := r.Cookie(cookie); err == nil && slices.Contains(allowed, c.Value) {
		return c.Value
	}
	return allowed[0]
}

// savePrefs handles POST /prefs: theme, size and next (where to go back to).
func (s *Server) savePrefs(w http.ResponseWriter, r *http.Request) {
	expires := time.Now().Add(prefsTTL)
	if v := r.PostFormValue("theme"); slices.Contains(themes, v) {
		http.SetCookie(w, s.auth.Cookie(themeCookie, v, "/", expires))
	}
	if v := r.PostFormValue("size"); slices.Contains(textSizes, v) {
		http.SetCookie(w, s.auth.Cookie(sizeCookie, v, "/", expires))
	}
	next := auth.SafeNext(r.PostFormValue("next"))
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther) // #nosec G710 -- next passed SafeNext: local paths only, no backslashes
}
