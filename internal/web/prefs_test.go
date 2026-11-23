package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestPrefs(t *testing.T) {
	e := newEnv(t)
	c := e.anon() // works logged out

	page := c.get("/login")
	if !strings.Contains(page.Body, `data-theme="system" data-size="normal"`) {
		t.Fatal("defaults not rendered on <html>")
	}
	if !strings.Contains(page.Body, `<option value="us" selected>`) || !strings.Contains(page.Body, `<option value="12h" selected>`) {
		t.Fatal("dates and clock should default to US and 12-hour")
	}
	if !strings.Contains(page.Body, `name="next" value="/login"`) {
		t.Fatal("prefs form doesn't return to the current page")
	}
	// The display settings sit in the header, not the footer.
	header := page.Body[strings.Index(page.Body, "<header"):strings.Index(page.Body, "</header>")]
	footer := page.Body[strings.Index(page.Body, "<footer"):strings.Index(page.Body, "</footer>")]
	if !strings.Contains(header, `action="/prefs"`) || strings.Contains(footer, `action="/prefs"`) {
		t.Fatal("prefs form should be in the header only")
	}

	r := c.post("/prefs", url.Values{"theme": {"dark"}, "size": {"huge"}, "dates": {"iso"}, "clock": {"24h"}, "next": {"/login"}})
	if r.Status != http.StatusSeeOther || r.Location != "/login" {
		t.Fatalf("save prefs: %d %s", r.Status, r.Location)
	}
	for _, want := range []string{"ots_theme=dark", "ots_size=huge", "ots_dates=iso", "ots_clock=24h"} {
		found := false
		for _, sc := range r.Header.Values("Set-Cookie") {
			if strings.HasPrefix(sc, want) && strings.Contains(sc, "SameSite=Lax") {
				found = true
			}
		}
		if !found {
			t.Errorf("cookie %s not set: %v", want, r.Header.Values("Set-Cookie"))
		}
	}
	page = c.get("/login")
	if !strings.Contains(page.Body, `data-theme="dark" data-size="huge"`) {
		t.Fatal("chosen prefs not applied")
	}
	for _, want := range []string{"dark", "huge", "iso", "24h"} {
		if !strings.Contains(page.Body, `<option value="`+want+`" selected>`) {
			t.Fatalf("prefs form doesn't show the current choice %q", want)
		}
	}

	// Unknown values are ignored (the old choice stays) and off-site next
	// falls back to home.
	r = c.post("/prefs", url.Values{"theme": {"neon"}, "size": {"x"}, "dates": {"eu"}, "clock": {"36h"}, "next": {"//evil.example"}})
	if r.Location != "/" {
		t.Fatalf("off-site next followed: %s", r.Location)
	}
	if page = c.get("/login"); !strings.Contains(page.Body, `data-theme="dark" data-size="huge"`) ||
		!strings.Contains(page.Body, `<option value="iso" selected>`) || !strings.Contains(page.Body, `<option value="24h" selected>`) {
		t.Fatal("bad values changed prefs")
	}

	// A tampered cookie falls back to the defaults.
	r = e.anon().do("GET", "/login", nil, map[string]string{"Cookie": "ots_theme=evil; ots_size=99; ots_dates=x; ots_clock=y"})
	if !strings.Contains(r.Body, `data-theme="system" data-size="normal"`) ||
		!strings.Contains(r.Body, `<option value="us" selected>`) || !strings.Contains(r.Body, `<option value="12h" selected>`) {
		t.Fatal("tampered cookie not rejected")
	}
}

func TestPrefsWorkLoggedIn(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	g := ana.createGroup("Lisbon", "EUR")
	path := "/groups/" + itoa(g)
	if r := ana.get(path); !strings.Contains(r.Body, `name="next" value="`+path+`"`) {
		t.Fatal("prefs form doesn't return to the group page")
	}
	ana.mustPost("/prefs", url.Values{"theme": {"light"}, "size": {"large"}, "next": {path}})
	if r := ana.get(path); !strings.Contains(r.Body, `data-theme="light" data-size="large"`) {
		t.Fatal("prefs not applied on the group page")
	}
}

func TestUserMenu(t *testing.T) {
	e := newEnv(t)
	menu := func(body string) string {
		t.Helper()
		i, j := strings.Index(body, `<details class="user-menu">`), strings.Index(body, "</header>")
		if i < 0 || j < i {
			t.Fatalf("no user menu in the header:\n%s", body)
		}
		return body[i:j]
	}

	// Logged in: the name opens a menu with Account, the display settings
	// and Log out, in that order; none of them are left in the main nav.
	body := e.login("ana").get("/groups").Body
	m := menu(body)
	rest := m
	for _, want := range []string{"<summary>ana</summary>", `href="/account"`, `action="/prefs"`, `name="dates"`, `name="clock"`, `action="/logout"`} {
		k := strings.Index(rest, want)
		if k < 0 {
			t.Fatalf("user menu missing %q (or out of order):\n%s", want, m)
		}
		rest = rest[k+len(want):]
	}
	nav := body[strings.Index(body, `<nav aria-label="Main">`):strings.Index(body, "</nav>")]
	for _, gone := range []string{`href="/account"`, `action="/logout"`, `action="/prefs"`} {
		if strings.Contains(nav, gone) {
			t.Errorf("main nav still has %s", gone)
		}
	}

	// Logged out: the settings still work, under a generic label.
	m = menu(e.anon().get("/login").Body)
	if !strings.Contains(m, "<summary>Settings</summary>") || !strings.Contains(m, `action="/prefs"`) {
		t.Errorf("logged-out menu should offer the settings:\n%s", m)
	}
	if strings.Contains(m, `action="/logout"`) || strings.Contains(m, `href="/account"`) {
		t.Errorf("logged-out menu offers account or logout:\n%s", m)
	}
}
