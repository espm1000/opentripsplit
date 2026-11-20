package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func (e *env) setupToken() string {
	tok, err := e.srv.currentSetupToken(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func withToken(tok string, v url.Values) url.Values {
	out := url.Values{"setup_token": {tok}}
	for k, vs := range v {
		out[k] = vs
	}
	return out
}

func TestSetupWithoutEnvVars(t *testing.T) {
	e := newEnvWith(t, nil)
	owner := e.anon()

	if r := owner.get("/login"); !strings.Contains(r.Body, "no login method yet") {
		t.Fatal("login page should point to setup when no provider is configured")
	}
	if r := owner.get("/setup"); r.Status != 200 || !strings.Contains(r.Body, "/auth/github/callback") {
		t.Fatalf("setup page: %d", r.Status)
	}
	tok := e.setupToken()
	if tok == "" {
		t.Fatal("no setup token while there is no admin")
	}

	githubCreds := url.Values{"github_client_id": {"gh-ui-id"}, "github_client_secret": {"gh-ui-secret"}}
	if r := owner.post("/setup", withToken("wrong", githubCreds)); r.Status != http.StatusForbidden {
		t.Fatalf("wrong token: %d", r.Status)
	}
	if !strings.Contains(e.logs.String(), "wrong setup token") {
		t.Fatal("wrong setup token not logged")
	}
	if r := owner.post("/setup", withToken(tok, nil)); r.Status != http.StatusBadRequest || !strings.Contains(r.Body, "at least one") {
		t.Fatalf("no providers: %d", r.Status)
	}
	if r := owner.post("/setup", withToken(tok, url.Values{"github_client_id": {"x"}})); r.Status != http.StatusBadRequest || !strings.Contains(r.Body, "client secret") {
		t.Fatalf("missing secret: %d", r.Status)
	}
	if len(e.srv.auth.Providers()) != 0 {
		t.Fatal("failed setup enabled a provider")
	}

	if r := owner.post("/setup", withToken(tok, githubCreds)); r.Status != http.StatusSeeOther || r.Location != "/login" {
		t.Fatalf("setup: %d %s", r.Status, r.Location)
	}
	if p := e.srv.auth.Provider("github"); p == nil || p.Config.ClientID != "gh-ui-id" {
		t.Fatal("GitHub provider not enabled from the UI")
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "login providers updated") || !strings.Contains(logs, "on=[github]") || !strings.Contains(logs, "off=[discord]") {
		t.Fatalf("provider change not logged:\n%s", logs)
	}
	if strings.Contains(logs, "gh-ui-secret") || strings.Contains(logs, "gh-ui-id") {
		t.Fatal("credentials were logged")
	}

	// Someone else logs in between setup and the owner's login: they must
	// not become admin, because they don't have the setup cookie.
	bo := e.login("bo")
	if u, _ := e.store.User(context.Background(), bo.ID); u.IsAdmin {
		t.Fatal("login without the setup cookie became admin")
	}

	owner.loginAs("owner")
	if u, _ := e.store.User(context.Background(), owner.ID); !u.IsAdmin {
		t.Fatal("setup completer is not admin")
	}
	if e.setupToken() != "" {
		t.Fatal("setup token still valid after an admin exists")
	}
	if r := owner.get("/setup"); r.Status != http.StatusNotFound {
		t.Fatalf("setup after completion: %d", r.Status)
	}
	if r := bo.post("/setup", withToken(tok, githubCreds)); r.Status != http.StatusNotFound {
		t.Fatalf("reusing the old setup token: %d", r.Status)
	}
	if r := owner.get("/groups"); !strings.Contains(r.Body, `href="/admin"`) {
		t.Fatal("admin link missing from nav")
	}
	if r := bo.get("/admin"); r.Status != http.StatusNotFound {
		t.Fatalf("non-admin GET /admin: %d", r.Status)
	}
	if r := bo.post("/admin/providers", url.Values{"github_client_id": {"evil"}}); r.Status != http.StatusNotFound {
		t.Fatalf("non-admin save: %d", r.Status)
	}
}

func TestAdminManagesProviders(t *testing.T) {
	e := newEnvWith(t, nil)
	admin := e.anon()
	admin.mustPost("/setup", withToken(e.setupToken(), url.Values{"github_client_id": {"g1"}, "github_client_secret": {"s1"}}))
	admin.loginAs("admin")

	page := admin.get("/admin")
	if page.Status != 200 || strings.Contains(page.Body, "s1") {
		t.Fatalf("admin page: %d (the secret must never be rendered)", page.Status)
	}

	// Turn on Discord; a blank GitHub secret keeps the saved one.
	r := admin.post("/admin/providers", url.Values{
		"github_client_id": {"g1"}, "discord_client_id": {"d1"}, "discord_client_secret": {"s2"},
	})
	if r.Status != http.StatusSeeOther {
		t.Fatalf("save: %d %s", r.Status, r.Body)
	}
	if p := e.srv.auth.Provider("github"); p == nil || p.Config.ClientSecret != "s1" {
		t.Fatal("GitHub secret not kept")
	}
	c := e.anon()
	e.discord.Next = "dee"
	if resp := c.follow("/auth/discord/login"); resp.Request.URL.Path != "/groups" {
		t.Fatalf("Discord login after enabling: %s", resp.Request.URL)
	}

	// Turning everything off is refused.
	if r := admin.post("/admin/providers", url.Values{}); r.Status != http.StatusBadRequest {
		t.Fatalf("disable all: %d", r.Status)
	}
	// Turn Discord off again.
	admin.mustPost("/admin/providers", url.Values{"github_client_id": {"g1"}})
	if r := e.anon().get("/auth/discord/login"); r.Status != http.StatusNotFound {
		t.Fatalf("Discord login after disabling: %d", r.Status)
	}
	settings, _ := e.store.Settings(context.Background())
	if _, ok := settings["oauth.discord.client_secret"]; ok {
		t.Fatal("disabled provider's secret kept in the database")
	}
}

func TestEnvProvidersAreReadOnlyInUI(t *testing.T) {
	e := newEnv(t) // both providers from env vars
	admin := e.anon()
	// With env providers, setup needs only the token.
	admin.mustPost("/setup", withToken(e.setupToken(), nil))
	admin.loginAs("admin")

	page := admin.get("/admin")
	if !strings.Contains(page.Body, "GITHUB_CLIENT_ID") || strings.Contains(page.Body, `name="github_client_id"`) {
		t.Fatal("env provider should be shown read-only")
	}
	admin.mustPost("/admin/providers", url.Values{"github_client_id": {"evil"}, "github_client_secret": {"evil"}})
	if p := e.srv.auth.Provider("github"); p.Config.ClientID != "gh-id" {
		t.Fatalf("env provider overridden from the UI: %s", p.Config.ClientID)
	}
	if settings, _ := e.store.Settings(context.Background()); len(settings) != 0 {
		t.Fatalf("settings written for env providers: %v", settings)
	}
}

func TestUIProvidersLoadAtStartup(t *testing.T) {
	e := newEnvWith(t, nil)
	e.anon().mustPost("/setup", withToken(e.setupToken(), url.Values{"discord_client_id": {"d1"}, "discord_client_secret": {"s"}}))
	// A fresh server on the same database picks them up.
	srv, err := New(context.Background(), e.store, e.srv.auth, Options{BaseURL: e.srv.baseURL, Log: e.srv.log})
	if err != nil {
		t.Fatal(err)
	}
	if p := srv.auth.Provider("discord"); p == nil || p.Config.ClientID != "d1" {
		t.Fatal("UI-configured provider not loaded at startup")
	}
}
