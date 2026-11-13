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
	if !strings.Contains(page.Body, `name="next" value="/login"`) {
		t.Fatal("prefs form doesn't return to the current page")
	}

	r := c.post("/prefs", url.Values{"theme": {"dark"}, "size": {"huge"}, "next": {"/login"}})
	if r.Status != http.StatusSeeOther || r.Location != "/login" {
		t.Fatalf("save prefs: %d %s", r.Status, r.Location)
	}
	for _, want := range []string{"ots_theme=dark", "ots_size=huge"} {
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
	if !strings.Contains(page.Body, `<option value="dark" selected>`) || !strings.Contains(page.Body, `<option value="huge" selected>`) {
		t.Fatal("prefs form doesn't show the current choice")
	}

	// Unknown values are ignored (the old choice stays) and off-site next
	// falls back to home.
	r = c.post("/prefs", url.Values{"theme": {"neon"}, "size": {"x"}, "next": {"//evil.example"}})
	if r.Location != "/" {
		t.Fatalf("off-site next followed: %s", r.Location)
	}
	if page = c.get("/login"); !strings.Contains(page.Body, `data-theme="dark" data-size="huge"`) {
		t.Fatal("bad values changed prefs")
	}

	// A tampered cookie falls back to the defaults.
	r = e.anon().do("GET", "/login", nil, map[string]string{"Cookie": "ots_theme=evil; ots_size=99"})
	if !strings.Contains(r.Body, `data-theme="system" data-size="normal"`) {
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
