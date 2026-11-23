package web

import (
	"net/http"
	"slices"
	"time"

	"github.com/espm1000/opentripsplit/internal/auth"
)

// Display preferences (colour theme, text size, date and clock format) are per browser, kept
// in cookies and applied on the server so the page never flashes the wrong
// theme. They work logged out too.

const (
	themeCookie = "ots_theme"
	sizeCookie  = "ots_size"
	datesCookie = "ots_dates"
	clockCookie = "ots_clock"
	prefsTTL    = 365 * 24 * time.Hour
)

var (
	themes      = []string{"system", "dark", "light"}
	textSizes   = []string{"normal", "large", "huge"}
	dateStyles  = []string{"us", "iso"}
	clockStyles = []string{"12h", "24h"}
)

// timeFmt formats dates and times the way the viewer chose: US
// (01/02/2006) or ISO (2006-01-02) dates, and a 12- or 24-hour clock.
type timeFmt struct {
	Dates string // us or iso
	Clock string // 12h or 24h
}

// Date formats the date part of t.
func (f timeFmt) Date(t time.Time) string {
	if f.Dates == "iso" {
		return t.Format(time.DateOnly)
	}
	return t.Format("01/02/2006")
}

// Day reformats a stored YYYY-MM-DD date, or returns it unchanged if it
// doesn't parse.
func (f timeFmt) Day(s string) string {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return s
	}
	return f.Date(t)
}

// Stamp formats t as a date and a time with its zone.
func (f timeFmt) Stamp(t time.Time) string {
	clock := "3:04 PM MST"
	if f.Clock == "24h" {
		clock = "15:04 MST"
	}
	return f.Date(t) + " " + t.Format(clock)
}

// pref returns the cookie value if it's one of allowed, else allowed[0].
func pref(r *http.Request, cookie string, allowed []string) string {
	if c, err := r.Cookie(cookie); err == nil && slices.Contains(allowed, c.Value) {
		return c.Value
	}
	return allowed[0]
}

// savePrefs handles POST /prefs: theme, size, dates, clock and next (where
// to go back to).
func (s *Server) savePrefs(w http.ResponseWriter, r *http.Request) {
	expires := time.Now().Add(prefsTTL)
	for _, p := range []struct {
		field, cookie string
		allowed       []string
	}{
		{"theme", themeCookie, themes},
		{"size", sizeCookie, textSizes},
		{"dates", datesCookie, dateStyles},
		{"clock", clockCookie, clockStyles},
	} {
		if v := r.PostFormValue(p.field); slices.Contains(p.allowed, v) {
			http.SetCookie(w, s.auth.Cookie(p.cookie, v, "/", expires))
		}
	}
	next := auth.SafeNext(r.PostFormValue("next"))
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther) // #nosec G710 -- next passed SafeNext: local paths only, no backslashes
}
