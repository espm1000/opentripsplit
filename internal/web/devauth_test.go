package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func newDevEnv(t *testing.T) *env {
	return newEnvWith(t, nil, func(o *Options) { o.DevLocalAuth = true })
}

// devLogin logs this browser in with the dev login form.
func (c *client) devLogin(form url.Values) result {
	c.e.t.Helper()
	r := c.post("/dev/login", form)
	if r.Status == http.StatusSeeOther {
		if u, err := c.sessionUser(); err == nil {
			c.ID = u.ID
		}
	}
	return r
}

func TestDevLoginOffByDefault(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	if r := c.post("/dev/login", url.Values{"user": {"alice"}}); r.Status != http.StatusNotFound {
		t.Fatalf("dev login when off: %d", r.Status)
	}
	ana := e.login("ana")
	g := ana.createGroup("Trip", "EUR")
	if r := ana.post(fmt.Sprintf("/groups/%d/dev/add-users", g), nil); r.Status != http.StatusNotFound {
		t.Fatalf("dev add-users when off: %d", r.Status)
	}
	if r := c.get("/login"); strings.Contains(r.Body, "Dev login") {
		t.Fatal("dev login shown when off")
	}
}

func TestDevLogin(t *testing.T) {
	e := newDevEnv(t)
	c := e.anon()

	page := c.get("/login")
	for _, want := range []string{"Dev login is on", "Log in as Alice (dev)", "Log in as Bob (dev)", "Log in as Casey (dev)"} {
		if !strings.Contains(page.Body, want) {
			t.Errorf("login page missing %q", want)
		}
	}
	if strings.Contains(page.Body, "no login method yet") {
		t.Error("setup notice shown although dev login works")
	}

	if r := c.devLogin(url.Values{"user": {"alice"}}); r.Status != http.StatusSeeOther || r.Location != "/groups" {
		t.Fatalf("dev login: %d %s", r.Status, r.Location)
	}
	alice, _ := c.sessionUser()
	if alice.DisplayName != "Alice (dev)" {
		t.Fatalf("logged in as %q", alice.DisplayName)
	}
	if r := c.get("/groups"); !strings.Contains(r.Body, "Switch user") {
		t.Error("switch user link missing")
	}

	// Switching: the login page stays reachable, and the old session ends.
	if r := c.get("/login?switch=1"); r.Status != 200 {
		t.Fatalf("switch page: %d", r.Status)
	}
	c.devLogin(url.Values{"user": {"bob"}, "next": {"/account"}})
	bob, _ := c.sessionUser()
	if bob.DisplayName != "Bob (dev)" || bob.ID == alice.ID {
		t.Fatalf("switched to %+v", bob)
	}

	// Free-form names map to a stable account.
	other := e.anon()
	other.devLogin(url.Values{"name": {"  Nick  "}})
	first := other.ID
	other.devLogin(url.Values{"name": {"nick"}})
	if other.ID != first {
		t.Fatal("same name gave a different account")
	}

	// Unknown preset, empty form, and off-site next are handled.
	if r := e.anon().post("/dev/login", url.Values{"user": {"mallory"}}); r.Status != http.StatusBadRequest {
		t.Fatalf("unknown dev user: %d", r.Status)
	}
	if r := e.anon().devLogin(url.Values{"user": {"casey"}, "next": {"//evil.example"}}); r.Location != "/groups" {
		t.Fatalf("unsafe next followed: %s", r.Location)
	}
}

func TestDevAddUsersToGroup(t *testing.T) {
	e := newDevEnv(t)
	me := e.anon()
	me.devLogin(url.Values{"name": {"Nick"}})
	g := me.createGroup("Lisbon", "EUR")

	me.mustPost(fmt.Sprintf("/groups/%d/dev/add-users", g), nil)
	ms, _ := e.store.Members(context.Background(), g)
	if len(ms) != 4 {
		t.Fatalf("members = %d, want 4", len(ms))
	}
	// Running it again doesn't duplicate anyone or leave invites open.
	me.mustPost(fmt.Sprintf("/groups/%d/dev/add-users", g), nil)
	if ms, _ := e.store.Members(context.Background(), g); len(ms) != 4 {
		t.Fatalf("members after second run = %d", len(ms))
	}
	if invs, _ := e.store.ActiveInvites(context.Background(), g); len(invs) != 0 {
		t.Fatalf("open invites left: %d", len(invs))
	}

	// The dev users can log in and use the group.
	bob := e.anon()
	bob.devLogin(url.Values{"user": {"bob"}})
	if r := bob.get(fmt.Sprintf("/groups/%d", g)); r.Status != 200 || !strings.Contains(r.Body, "Casey (dev)") {
		t.Fatalf("bob's view of the group: %d", r.Status)
	}
	if r := bob.addEqualExpense(g, "Coffee", "9.00", bob.ID, bob.ID, me.ID); r.Status != http.StatusSeeOther {
		t.Fatalf("bob adds expense: %d %s", r.Status, r.Body)
	}

	// Only the owner may add them, and only members can reach the route.
	if r := bob.post(fmt.Sprintf("/groups/%d/dev/add-users", g), nil); r.Status != http.StatusForbidden {
		t.Fatalf("non-owner add-users: %d", r.Status)
	}
	stranger := e.anon()
	stranger.devLogin(url.Values{"name": {"Stranger"}})
	if r := stranger.post(fmt.Sprintf("/groups/%d/dev/add-users", g), nil); r.Status != http.StatusNotFound {
		t.Fatalf("non-member add-users: %d", r.Status)
	}
}

func TestDevLoginCanClaimAdmin(t *testing.T) {
	e := newDevEnv(t)
	c := e.anon()
	c.mustPost("/setup", withToken(e.setupToken(), url.Values{"github_client_id": {"g"}, "github_client_secret": {"s"}}))
	// A dev login from the browser that completed setup becomes admin, so
	// the admin pages can be tested without an OAuth app.
	c.devLogin(url.Values{"name": {"Nick"}})
	if u, _ := c.sessionUser(); !u.IsAdmin {
		t.Fatal("dev login after setup didn't claim admin")
	}
	if r := c.get("/admin"); r.Status != 200 {
		t.Fatalf("admin page: %d", r.Status)
	}
	// Other dev users don't.
	bob := e.anon()
	bob.devLogin(url.Values{"user": {"bob"}})
	if u, _ := bob.sessionUser(); u.IsAdmin {
		t.Fatal("second dev user became admin")
	}
}
