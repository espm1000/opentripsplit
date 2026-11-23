package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/espm1000/opentripsplit/internal/auth"
	"github.com/espm1000/opentripsplit/internal/config"
	"github.com/espm1000/opentripsplit/internal/store"
)

// fakeProvider is a minimal OAuth2 server with PKCE. The test sets Next to
// choose which account the next authorization logs in as.
type fakeProvider struct {
	mu      sync.Mutex
	Next    string
	codes   map[string]fakeGrant
	profile func(login string) any
	srv     *httptest.Server
}

type fakeGrant struct{ login, challenge string }

func newFakeProvider(t *testing.T, profile func(string) any) *fakeProvider {
	f := &fakeProvider{codes: map[string]fakeGrant{}, profile: profile}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "PKCE required", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		code := auth.NewToken()
		f.codes[code] = fakeGrant{f.Next, q.Get("code_challenge")}
		f.mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+code+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		g, ok := f.codes[r.PostFormValue("code")]
		delete(f.codes, r.PostFormValue("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "tok-" + g.login, "token_type": "bearer"})
	})
	mux.HandleFunc("GET /user", func(w http.ResponseWriter, r *http.Request) {
		login, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer tok-")
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(f.profile(login))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeProvider) wire(p *auth.Provider) *auth.Provider {
	p.Config.Endpoint = oauth2.Endpoint{AuthURL: f.srv.URL + "/authorize", TokenURL: f.srv.URL + "/token", AuthStyle: oauth2.AuthStyleInParams}
	p.UserURL = f.srv.URL + "/user"
	return p
}

type env struct {
	t       *testing.T
	store   *store.Store
	srv     *Server
	ts      *httptest.Server
	github  *fakeProvider
	discord *fakeProvider
	logs    *logBuffer
}

// logBuffer collects server logs. Handlers log from the server's
// goroutines, so access is locked.
type logBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newEnv starts a server with both providers configured through env vars.
func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, map[string]config.OAuthCreds{
		"github":  {ClientID: "gh-id", ClientSecret: "gh-secret"},
		"discord": {ClientID: "dc-id", ClientSecret: "dc-secret"},
	})
}

// newEnvWith starts a server with the given env-var providers (none means
// the instance must be set up at /setup).
func newEnvWith(t *testing.T, envProviders map[string]config.OAuthCreds, opts ...func(*Options)) *env {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	e := &env{t: t, store: st, logs: &logBuffer{}}
	// GitHub profiles: numeric id derived from the login.
	e.github = newFakeProvider(t, func(login string) any {
		var id int64
		for _, c := range login {
			id = id*31 + int64(c)
		}
		return map[string]any{"id": id, "login": login, "email": "should-not-be-stored@example.com"}
	})
	e.discord = newFakeProvider(t, func(login string) any {
		return map[string]any{"id": "d-" + login, "username": login, "global_name": nil}
	})

	var h http.Handler
	e.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h.ServeHTTP(w, r) }))
	t.Cleanup(e.ts.Close)
	base, _ := url.Parse(e.ts.URL)
	log := slog.New(slog.NewTextHandler(e.logs, nil))
	a := &auth.Auth{Store: st, SessionTTL: 30 * 24 * time.Hour, Log: log}
	o := Options{BaseURL: base, InviteTTL: 7 * 24 * time.Hour, EnvProviders: envProviders, Log: log}
	for _, opt := range opts {
		opt(&o)
	}
	e.srv, err = New(context.Background(), st, a, o)
	if err != nil {
		t.Fatal(err)
	}
	// Point every provider (env or UI-configured) at the fake servers.
	e.srv.wrapProvider = func(p *auth.Provider) {
		map[string]*fakeProvider{"github": e.github, "discord": e.discord}[p.Name].wire(p)
	}
	if err := e.srv.reloadProviders(context.Background()); err != nil {
		t.Fatal(err)
	}
	h = e.srv.Handler()
	return e
}

// client is a browser for one person: it keeps cookies and does not follow
// redirects, so tests can check status codes.
type client struct {
	e    *env
	http *http.Client
	ID   int64
}

func (e *env) anon() *client {
	jar, _ := cookiejar.New(nil)
	return &client{e: e, http: &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// login runs the whole OAuth flow against the fake GitHub.
func (e *env) login(name string) *client {
	e.t.Helper()
	c := e.anon()
	c.loginAs(name)
	return c
}

// loginAs logs this browser in through the fake GitHub.
func (c *client) loginAs(name string) {
	c.e.t.Helper()
	c.e.github.Next = name
	resp := c.follow("/auth/github/login")
	if resp.Request.URL.Path != "/groups" {
		c.e.t.Fatalf("login %s ended at %s (%d)", name, resp.Request.URL, resp.StatusCode)
	}
	u, err := c.sessionUser()
	if err != nil {
		c.e.t.Fatal(err)
	}
	c.ID = u.ID
}

func (c *client) sessionUser() (store.User, error) {
	base, _ := url.Parse(c.e.ts.URL)
	for _, ck := range c.http.Jar.Cookies(base) {
		if ck.Name == "ots_session" {
			return c.e.store.SessionUser(context.Background(), auth.HashToken(ck.Value))
		}
	}
	return store.User{}, fmt.Errorf("no session cookie")
}

// follow GETs path and follows redirects (including to the fake provider).
func (c *client) follow(path string) *http.Response {
	c.e.t.Helper()
	fc := &http.Client{Jar: c.http.Jar}
	resp, err := fc.Get(c.e.ts.URL + path)
	if err != nil {
		c.e.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

type result struct {
	Status   int
	Body     string
	Location string
	Header   http.Header
}

func (c *client) do(method, path string, form url.Values, hdr map[string]string) result {
	c.e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, c.e.ts.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, string(b), resp.Header.Get("Location"), resp.Header}
}

func (c *client) get(path string) result { return c.do("GET", path, nil, nil) }
func (c *client) post(path string, form url.Values) result {
	if form == nil {
		form = url.Values{}
	}
	return c.do("POST", path, form, nil)
}

func (c *client) mustPost(path string, form url.Values) result {
	c.e.t.Helper()
	r := c.post(path, form)
	if r.Status != http.StatusSeeOther && r.Status != http.StatusOK {
		c.e.t.Fatalf("POST %s: %d\n%s", path, r.Status, r.Body)
	}
	return r
}

func (c *client) createGroup(name, currency string) int64 {
	c.e.t.Helper()
	r := c.mustPost("/groups", url.Values{"name": {name}, "currency": {currency}})
	id, err := strconv.ParseInt(strings.TrimPrefix(r.Location, "/groups/"), 10, 64)
	if err != nil {
		c.e.t.Fatalf("create group redirect %q", r.Location)
	}
	return id
}

var inviteRe = regexp.MustCompile(`/invite/([A-Za-z0-9_-]+)`)

func (c *client) invite(g int64) string {
	c.e.t.Helper()
	r := c.mustPost(fmt.Sprintf("/groups/%d/invites", g), nil)
	m := inviteRe.FindStringSubmatch(r.Body)
	if m == nil {
		c.e.t.Fatalf("no invite link in page:\n%s", r.Body)
	}
	return m[1]
}

func (c *client) join(token string) {
	c.e.t.Helper()
	c.mustPost("/invite/"+token, nil)
}

func (c *client) addEqualExpense(g int64, desc, amount string, payer int64, users ...int64) result {
	form := url.Values{
		"description": {desc}, "amount": {amount}, "payer_id": {strconv.FormatInt(payer, 10)},
		"spent_on": {"2026-07-01"}, "split_type": {"equal"},
	}
	for _, u := range users {
		form.Set("include_"+strconv.FormatInt(u, 10), "1")
	}
	return c.post(fmt.Sprintf("/groups/%d/expenses", g), form)
}

func TestLoginFlow(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")

	r := ana.get("/groups")
	if r.Status != 200 || !strings.Contains(r.Body, "ana") {
		t.Fatalf("groups page: %d", r.Status)
	}
	// No email stored anywhere: the user row has only the login as name.
	u, _ := e.store.User(context.Background(), ana.ID)
	if u.DisplayName != "ana" {
		t.Fatalf("display name = %q", u.DisplayName)
	}

	// Logging in again reaches the same user and rotates the session.
	old, _ := ana.sessionUser()
	e.github.Next = "ana"
	ana.follow("/auth/github/login")
	again, err := ana.sessionUser()
	if err != nil || again.ID != old.ID {
		t.Fatalf("relogin user = %+v, %v", again, err)
	}

	// Logout deletes the session.
	if r := ana.post("/logout", nil); r.Status != http.StatusSeeOther {
		t.Fatalf("logout: %d", r.Status)
	}
	if r := ana.get("/groups"); r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/login") {
		t.Fatalf("after logout: %d %s", r.Status, r.Location)
	}
}

func TestLoginRejectsBadState(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	r := c.get("/auth/github/login")
	if r.Status != http.StatusFound {
		t.Fatalf("login start: %d", r.Status)
	}
	loc, _ := url.Parse(r.Location)
	if loc.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("no PKCE challenge in auth URL")
	}
	// Lax, not Strict, or the browser drops it on the provider's redirect back.
	if sc := r.Header.Get("Set-Cookie"); !strings.Contains(sc, "SameSite=Lax") || !strings.Contains(sc, "HttpOnly") {
		t.Fatalf("flow cookie flags: %s", sc)
	}
	if r := c.get("/auth/github/callback?code=x&state=wrong"); r.Status != http.StatusBadRequest {
		t.Fatalf("bad state: %d", r.Status)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "login state missing or mismatched") || !strings.Contains(logs, "provider=github") {
		t.Fatalf("bad state not logged:\n%s", logs)
	}

	// The provider sends the user back with an error instead of a code.
	r = c.get("/auth/github/login")
	loc, _ = url.Parse(r.Location)
	state := loc.Query().Get("state")
	cb := "/auth/github/callback?" + url.Values{
		"state": {state}, "error": {"redirect_uri_mismatch"}, "error_description": {"The redirect_uri is not valid"},
	}.Encode()
	if r := c.get(cb); r.Status != http.StatusBadRequest {
		t.Fatalf("provider error: %d", r.Status)
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "error=redirect_uri_mismatch") || !strings.Contains(logs, "The redirect_uri is not valid") {
		t.Fatalf("provider error not logged:\n%s", logs)
	}
	if strings.Contains(logs, state) {
		t.Fatal("state was logged")
	}
}

func TestSessionCookieFlags(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	e.github.Next = "ana"
	r := c.get("/auth/github/login")
	loc, _ := url.Parse(r.Location)
	// Follow the provider redirect manually to see the callback response.
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Get(loc.String())
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	cb := resp.Header.Get("Location")
	cbURL, _ := url.Parse(cb)
	res := c.get(cbURL.RequestURI())
	var session string
	for _, sc := range res.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "ots_session=") {
			session = sc
		}
	}
	for _, want := range []string{"HttpOnly", "SameSite=Lax", "Path=/"} {
		if !strings.Contains(session, want) {
			t.Errorf("session cookie %q missing %s", session, want)
		}
	}
	if strings.Contains(session, "Secure") {
		t.Errorf("Secure set on http base URL: %q", session)
	}
}

func TestAccountLinking(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	e.discord.Next = "ana_dc"
	resp := ana.follow("/auth/discord/login?link=1")
	if resp.Request.URL.Path != "/account" {
		t.Fatalf("link ended at %s", resp.Request.URL)
	}
	ids, _ := e.store.Identities(context.Background(), ana.ID)
	if len(ids) != 2 {
		t.Fatalf("identities = %v", ids)
	}
	// A different user can't take that Discord account.
	bo := e.login("bo")
	e.discord.Next = "ana_dc"
	resp = bo.follow("/auth/discord/login?link=1")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("stealing link: %d", resp.StatusCode)
	}
	// Logging in with Discord now reaches ana's account.
	c := e.anon()
	e.discord.Next = "ana_dc"
	c.follow("/auth/discord/login")
	if u, err := c.sessionUser(); err != nil || u.ID != ana.ID {
		t.Fatalf("discord login = %+v, %v", u, err)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	r := e.anon().get("/login")
	for k, want := range map[string]string{
		"Content-Security-Policy": "default-src 'self'",
		"X-Content-Type-Options":  "nosniff",
		"Referrer-Policy":         "same-origin",
	} {
		if !strings.Contains(r.Header.Get(k), want) {
			t.Errorf("%s = %q", k, r.Header.Get(k))
		}
	}
	if strings.Contains(r.Body, "<script>") || strings.Contains(r.Body, "style=") {
		t.Error("inline script or style in page (blocked by CSP)")
	}
}

func TestCrossOriginPostBlocked(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	r := ana.do("POST", "/groups", url.Values{"name": {"x"}, "currency": {"USD"}}, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if r.Status != http.StatusForbidden {
		t.Fatalf("cross-site POST: %d", r.Status)
	}
	r = ana.do("POST", "/groups", url.Values{"name": {"x"}, "currency": {"USD"}}, map[string]string{"Origin": "https://evil.example"})
	if r.Status != http.StatusForbidden {
		t.Fatalf("foreign Origin POST: %d", r.Status)
	}
}

func TestAnonymousRedirectsToLoginWithNext(t *testing.T) {
	e := newEnv(t)
	r := e.anon().get("/invite/abc")
	if r.Status != http.StatusSeeOther || r.Location != "/login?next=%2Finvite%2Fabc" {
		t.Fatalf("anon invite: %d %s", r.Status, r.Location)
	}
}

// TestNextCantLeaveSite covers each place that redirects to a ?next= or
// next= value with a path http.Redirect would clean to "/\evil.example".
func TestNextCantLeaveSite(t *testing.T) {
	const evil = `/./\evil.example`
	e := newEnv(t)

	// After an OAuth login.
	c := e.anon()
	e.github.Next = "ana"
	if resp := c.follow("/auth/github/login?next=" + url.QueryEscape(evil)); resp.Request.URL.Path != "/groups" {
		t.Errorf("OAuth login with next=%s ended at %s", evil, resp.Request.URL)
	}
	// A logged-in visit to /login.
	if r := c.get("/login?next=" + url.QueryEscape(evil)); r.Status != http.StatusSeeOther || r.Location != "/groups" {
		t.Errorf("logged-in /login with next=%s: %d %s", evil, r.Status, r.Location)
	}
	// The theme and text-size form.
	if r := c.post("/prefs", url.Values{"theme": {"dark"}, "next": {evil}}); r.Status != http.StatusSeeOther || r.Location != "/" {
		t.Errorf("prefs with next=%s: %d %s", evil, r.Status, r.Location)
	}
}

func TestNonMemberGets404OnEveryGroupRoute(t *testing.T) {
	e := newEnv(t)
	ana, bo, eve := e.login("ana"), e.login("bo"), e.login("eve")
	g := ana.createGroup("Lisbon", "EUR")
	bo.join(ana.invite(g))
	if r := ana.addEqualExpense(g, "Dinner", "30", ana.ID, ana.ID, bo.ID); r.Status != http.StatusSeeOther {
		t.Fatalf("add expense: %d %s", r.Status, r.Body)
	}
	ana.mustPost(fmt.Sprintf("/groups/%d/settlements", g), url.Values{"from": {itoa(bo.ID)}, "to": {itoa(ana.ID)}, "amount": {"5"}})
	ctx := context.Background()
	es, _ := e.store.Expenses(ctx, g)
	ss, _ := e.store.Settlements(ctx, g)
	invs, _ := e.store.ActiveInvites(ctx, g)

	repl := strings.NewReplacer(
		"{id}", itoa(g), "{eid}", itoa(es[0].ID), "{sid}", itoa(ss[0].ID),
		"{uid}", itoa(bo.ID), "{iid}", itoa(invs[0].ID),
	)
	routes := e.srv.groupRoutes()
	if len(routes) < 18 {
		t.Fatalf("only %d group routes", len(routes))
	}
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.pattern, " ")
		path = repl.Replace(path)
		if r := eve.do(method, path, url.Values{}, nil); r.Status != http.StatusNotFound {
			t.Errorf("%s %s as non-member: %d", method, path, r.Status)
		}
		// Members get past the membership check (the edit form is 403 for
		// bo because ana created that expense).
		if method == "GET" {
			if r := bo.get(path); r.Status == http.StatusNotFound || r.Status >= 500 {
				t.Errorf("GET %s as member: %d", path, r.Status)
			}
		}
	}
	// Nothing changed.
	if es2, _ := e.store.Expenses(ctx, g); len(es2) != 1 {
		t.Fatal("non-member changed expenses")
	}
	// Unknown and malformed group IDs are 404 too.
	for _, p := range []string{"/groups/999999", "/groups/abc"} {
		if r := ana.get(p); r.Status != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, r.Status)
		}
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func TestLeaveRequiresZeroBalance(t *testing.T) {
	e := newEnv(t)
	ana, bo := e.login("ana"), e.login("bo")
	g := ana.createGroup("Lisbon", "EUR")
	bo.join(ana.invite(g))
	ana.addEqualExpense(g, "Dinner", "40.00", ana.ID, ana.ID, bo.ID)

	if r := bo.post(fmt.Sprintf("/groups/%d/leave", g), nil); r.Status != http.StatusConflict {
		t.Fatalf("leave with balance: %d", r.Status)
	}
	if r := ana.post(fmt.Sprintf("/groups/%d/members/%d/remove", g, bo.ID), nil); r.Status != http.StatusConflict {
		t.Fatalf("remove with balance: %d", r.Status)
	}
	// The settle-up suggestion is bo pays ana 20.00; mark it paid.
	page := bo.get(fmt.Sprintf("/groups/%d", g))
	if !strings.Contains(page.Body, "bo pays ana <strong>20.00\u00a0EUR</strong>") {
		t.Fatalf("settle-up suggestion missing:\n%s", page.Body)
	}
	bo.mustPost(fmt.Sprintf("/groups/%d/settlements", g), url.Values{"from": {itoa(bo.ID)}, "to": {itoa(ana.ID)}, "amount": {"20.00"}})
	if r := bo.post(fmt.Sprintf("/groups/%d/leave", g), nil); r.Status != http.StatusSeeOther || r.Location != "/groups" {
		t.Fatalf("leave with zero balance: %d %s", r.Status, r.Location)
	}
	if r := bo.get(fmt.Sprintf("/groups/%d", g)); r.Status != http.StatusNotFound {
		t.Fatalf("former member sees group: %d", r.Status)
	}
	if r := ana.post(fmt.Sprintf("/groups/%d/leave", g), nil); r.Status != http.StatusConflict {
		t.Fatalf("owner leave: %d", r.Status)
	}
}

func TestArchivedGroupRejectsWrites(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	g := ana.createGroup("Lisbon", "EUR")
	ana.addEqualExpense(g, "Dinner", "10", ana.ID, ana.ID)
	ana.mustPost(fmt.Sprintf("/groups/%d/archive", g), nil)

	es, _ := e.store.Expenses(context.Background(), g)
	writes := []string{
		fmt.Sprintf("/groups/%d/expenses", g),
		fmt.Sprintf("/groups/%d/expenses/%d", g, es[0].ID),
		fmt.Sprintf("/groups/%d/expenses/%d/delete", g, es[0].ID),
		fmt.Sprintf("/groups/%d/rename", g),
		fmt.Sprintf("/groups/%d/invites", g),
	}
	for _, p := range writes {
		form := url.Values{"name": {"x"}, "description": {"x"}, "amount": {"1"}, "payer_id": {itoa(ana.ID)},
			"spent_on": {"2026-07-01"}, "split_type": {"equal"}, "include_" + itoa(ana.ID): {"1"}}
		if r := ana.post(p, form); r.Status != http.StatusConflict {
			t.Errorf("POST %s on archived group: %d", p, r.Status)
		}
	}
	if r := ana.get(fmt.Sprintf("/groups/%d/expenses/new", g)); r.Status != http.StatusConflict {
		t.Errorf("new expense form on archived group: %d", r.Status)
	}
	page := ana.get(fmt.Sprintf("/groups/%d", g))
	if page.Status != 200 || !strings.Contains(page.Body, "Dinner") || strings.Contains(page.Body, "Add expense") {
		t.Fatal("archived group page should show history without add button")
	}
	// The owner settings start open on an archived group so Unarchive is in view.
	if !strings.Contains(page.Body, `<details class="fold" open>`) {
		t.Error("owner settings should start open on an archived group")
	}
	ana.mustPost(fmt.Sprintf("/groups/%d/unarchive", g), nil)
	if r := ana.addEqualExpense(g, "Taxi", "5", ana.ID, ana.ID); r.Status != http.StatusSeeOther {
		t.Fatalf("add after unarchive: %d", r.Status)
	}
}

func TestExpenseForm(t *testing.T) {
	e := newEnv(t)
	ana, bo := e.login("ana"), e.login("bo")
	g := ana.createGroup("Tokyo", "JPY")
	bo.join(ana.invite(g))
	path := fmt.Sprintf("/groups/%d/expenses", g)
	base := url.Values{"description": {"Sushi"}, "payer_id": {itoa(ana.ID)}, "spent_on": {"2026-07-02"}}

	// htmx fragment for the split type switch.
	frag := ana.get(fmt.Sprintf("/groups/%d/split-fields?split_type=percent", g))
	if frag.Status != 200 || !strings.Contains(frag.Body, `name="value_`+itoa(bo.ID)+`"`) || strings.Contains(frag.Body, "<html") {
		t.Fatalf("split-fields fragment: %d\n%s", frag.Status, frag.Body)
	}

	cases := []struct {
		name   string
		form   url.Values
		status int
		errMsg string
	}{
		{"JPY rejects decimals", url.Values{"amount": {"100.5"}, "split_type": {"equal"}, "include_" + itoa(ana.ID): {"1"}}, 400, "decimal places"},
		{"percent must sum to 100", url.Values{"amount": {"1000"}, "split_type": {"percent"}, "value_" + itoa(ana.ID): {"50"}, "value_" + itoa(bo.ID): {"40"}}, 400, "add up to 100"},
		{"nobody included", url.Values{"amount": {"1000"}, "split_type": {"equal"}}, 400, "at least one"},
		{"bad shares", url.Values{"amount": {"1000"}, "split_type": {"shares"}, "value_" + itoa(ana.ID): {"1.5"}}, 400, "whole numbers"},
		{"percent ok", url.Values{"amount": {"1000"}, "split_type": {"percent"}, "value_" + itoa(ana.ID): {"33.33"}, "value_" + itoa(bo.ID): {"66.67"}}, 303, ""},
	}
	for _, tc := range cases {
		form := url.Values{}
		for k, v := range base {
			form[k] = v
		}
		for k, v := range tc.form {
			form[k] = v
		}
		r := ana.post(path, form)
		if r.Status != tc.status || !strings.Contains(r.Body, tc.errMsg) {
			t.Errorf("%s: %d, body missing %q", tc.name, r.Status, tc.errMsg)
		}
		if tc.status == 400 && !strings.Contains(r.Body, `value="Sushi"`) {
			t.Errorf("%s: form values not kept on error", tc.name)
		}
	}
	es, _ := e.store.Expenses(context.Background(), g)
	if len(es) != 1 || es[0].Amount != 1000 || es[0].Shares[0].Input != 3333 {
		t.Fatalf("expenses = %+v", es)
	}
	// The edit form shows the stored percentages.
	edit := ana.get(fmt.Sprintf("/groups/%d/expenses/%d/edit", g, es[0].ID))
	if !strings.Contains(edit.Body, `value="33.33"`) || !strings.Contains(edit.Body, `value="66.67"`) {
		t.Fatalf("edit form missing percentages:\n%s", edit.Body)
	}
	// bo didn't create it and isn't owner.
	if r := bo.get(fmt.Sprintf("/groups/%d/expenses/%d/edit", g, es[0].ID)); r.Status != http.StatusForbidden {
		t.Fatalf("bo edit form: %d", r.Status)
	}
}

func TestInviteLinks(t *testing.T) {
	e := newEnv(t)
	ana, bo, cy := e.login("ana"), e.login("bo"), e.login("cy")
	g := ana.createGroup("Lisbon", "EUR")

	if r := bo.get("/invite/not-a-real-token"); r.Status != http.StatusNotFound {
		t.Fatalf("unknown invite: %d", r.Status)
	}
	tok := ana.invite(g)
	if r := bo.get("/invite/" + tok); r.Status != 200 || !strings.Contains(r.Body, "Lisbon") {
		t.Fatalf("invite page: %d", r.Status)
	}
	bo.join(tok)

	invs, _ := e.store.ActiveInvites(context.Background(), g)
	ana.mustPost(fmt.Sprintf("/groups/%d/invites/%d/revoke", g, invs[0].ID), nil)
	if r := cy.get("/invite/" + tok); r.Status != http.StatusGone {
		t.Fatalf("revoked invite page: %d", r.Status)
	}
	if r := cy.post("/invite/"+tok, nil); r.Status != http.StatusGone {
		t.Fatalf("revoked invite join: %d", r.Status)
	}

	// Expired: move the store clock past the 7-day invite TTL.
	tok2 := ana.invite(g)
	e.store.Now = func() time.Time { return time.Now().Add(8 * 24 * time.Hour) }
	if r := cy.post("/invite/"+tok2, nil); r.Status != http.StatusGone {
		t.Fatalf("expired invite: %d", r.Status)
	}
	if _, err := e.store.Membership(context.Background(), g, cy.ID); err == nil {
		t.Fatal("cy joined through a bad invite")
	}
}

func TestDeleteAccountHidesNameInHistory(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	e.github.Next = "Bo Secretname"
	bo := e.login("Bo Secretname")
	g := ana.createGroup("Lisbon", "EUR")
	bo.join(ana.invite(g))
	if r := bo.addEqualExpense(g, "Groceries", "12.00", bo.ID, ana.ID, bo.ID); r.Status != http.StatusSeeOther {
		t.Fatalf("add expense: %d", r.Status)
	}

	if r := bo.post("/account/delete", url.Values{"confirm": {"nope"}}); r.Status != http.StatusBadRequest {
		t.Fatalf("delete without confirm: %d", r.Status)
	}
	if r := bo.post("/account/delete", url.Values{"confirm": {"DELETE"}}); r.Status != 200 {
		t.Fatalf("delete account: %d", r.Status)
	}
	if _, err := bo.sessionUser(); err == nil {
		t.Fatal("session survived deletion")
	}

	former := store.FormerMemberName(bo.ID)
	for _, p := range []string{fmt.Sprintf("/groups/%d", g), fmt.Sprintf("/groups/%d/history", g)} {
		r := ana.get(p)
		if r.Status != 200 {
			t.Fatalf("GET %s: %d", p, r.Status)
		}
		if strings.Contains(r.Body, "Secretname") {
			t.Errorf("%s still shows the deleted user's name", p)
		}
		if !strings.Contains(r.Body, former) {
			t.Errorf("%s doesn't show %q", p, former)
		}
	}
	// Balances still add up: ana owes the former member 6.00.
	if r := ana.get(fmt.Sprintf("/groups/%d", g)); !strings.Contains(r.Body, "ana pays "+former+" <strong>6.00\u00a0EUR</strong>") {
		t.Fatal("settle-up with former member missing")
	}
}

// An expense that includes a former member can't be edited: the form only
// lists active members, so saving it would drop their share.
func TestExpenseWithFormerMemberIsLocked(t *testing.T) {
	e := newEnv(t)
	ana, bo := e.login("ana"), e.login("bo")
	g := ana.createGroup("Lisbon", "EUR")
	bo.join(ana.invite(g))
	if r := ana.addEqualExpense(g, "Dinner", "40.00", bo.ID, ana.ID, bo.ID); r.Status != http.StatusSeeOther {
		t.Fatalf("add expense: %d", r.Status)
	}
	es, _ := e.store.Expenses(context.Background(), g)
	editPath := fmt.Sprintf("/groups/%d/expenses/%d/edit", g, es[0].ID)
	if r := ana.get(fmt.Sprintf("/groups/%d", g)); !strings.Contains(r.Body, editPath) {
		t.Fatal("edit link missing while everyone is active")
	}
	bo.mustPost("/account/delete", url.Values{"confirm": {"DELETE"}})

	page := ana.get(fmt.Sprintf("/groups/%d", g))
	if strings.Contains(page.Body, editPath) {
		t.Error("edit link shown for an expense with a former member")
	}
	if !strings.Contains(page.Body, fmt.Sprintf("/groups/%d/expenses/%d/delete", g, es[0].ID)) {
		t.Error("delete button missing")
	}
	if r := ana.get(editPath); r.Status != http.StatusConflict || !strings.Contains(r.Body, "left the group") {
		t.Errorf("edit form: %d", r.Status)
	}
	// Posting the form directly is refused too, and nothing changes.
	form := url.Values{
		"description": {"Dinner fixed"}, "amount": {"40.00"}, "payer_id": {itoa(ana.ID)},
		"spent_on": {"2026-07-01"}, "split_type": {"equal"}, "include_" + itoa(ana.ID): {"1"},
	}
	if r := ana.post(fmt.Sprintf("/groups/%d/expenses/%d", g, es[0].ID), form); r.Status != http.StatusConflict {
		t.Errorf("update: %d", r.Status)
	}
	after, _ := e.store.Expense(context.Background(), g, es[0].ID)
	if after.Description != "Dinner" || after.PayerID != bo.ID || len(after.Shares) != 2 {
		t.Fatalf("expense changed: %+v", after)
	}
}

func TestHistoryShowsChanges(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	g := ana.createGroup("Lisbon", "EUR")
	ana.addEqualExpense(g, "Dinner", "10", ana.ID, ana.ID)
	es, _ := e.store.Expenses(context.Background(), g)
	ana.mustPost(fmt.Sprintf("/groups/%d/expenses/%d/delete", g, es[0].ID), nil)
	r := ana.get(fmt.Sprintf("/groups/%d/history", g))
	for _, want := range []string{"created the group", "added expense", "deleted expense", "&#34;Dinner&#34;"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("history missing %q", want)
		}
	}
}

func TestHealthz(t *testing.T) {
	e := newEnv(t)
	if r := e.anon().get("/healthz"); r.Status != 200 || r.Body != "ok\n" {
		t.Fatalf("healthz: %d %q", r.Status, r.Body)
	}
	e.anon().get("/login")
	// The test logger is at info level, like production without DEBUG.
	if logs := e.logs.String(); strings.Contains(logs, "path=/healthz") || !strings.Contains(logs, "path=/login") {
		t.Errorf("healthz should be logged at debug level only:\n%s", logs)
	}
}

func TestFooterVersion(t *testing.T) {
	e := newEnvWith(t, map[string]config.OAuthCreds{"github": {ClientID: "gh-id", ClientSecret: "gh-secret"}},
		func(o *Options) { o.Version = "1.2.3" })
	const want = `<div class="version">Version 1.2.3</div>`
	// Logged-out visitors don't learn which release is running.
	if r := e.anon().get("/login"); strings.Contains(r.Body, "1.2.3") {
		t.Errorf("login page shows the version to a logged-out visitor:\n%s", r.Body)
	}
	if r := e.login("ana").get("/groups"); !strings.Contains(r.Body, want) {
		t.Errorf("groups page footer lacks the version:\n%s", r.Body)
	}
}

func TestHealthzLogLevel(t *testing.T) {
	var buf strings.Builder
	s := &Server{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	status := http.StatusOK
	h := s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(buf.String(), "level=DEBUG") || !strings.Contains(buf.String(), "path=/healthz") {
		t.Errorf("successful healthz not logged at debug:\n%s", buf.String())
	}
	// A failing health check is worth seeing without DEBUG.
	buf.Reset()
	status = http.StatusServiceUnavailable
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	if !strings.Contains(buf.String(), "level=INFO") || !strings.Contains(buf.String(), "status=503") {
		t.Errorf("failing healthz not logged at info:\n%s", buf.String())
	}
}

func TestHighScoreBoard(t *testing.T) {
	e := newEnv(t)
	ana, bo, cy := e.login("ana"), e.login("bo"), e.login("cy")
	g := ana.createGroup("Vegas 2026", "USD")
	tok := ana.invite(g)
	bo.join(tok)
	cy.join(tok)
	// bo pays 90 for all three: bo +60, ana −30, cy −30. Then cy pays 12 for cy and ana.
	bo.addEqualExpense(g, "Buffet", "90", bo.ID, ana.ID, bo.ID, cy.ID)
	cy.addEqualExpense(g, "Cab", "12", cy.ID, ana.ID, cy.ID)
	// Balances: bo +60, cy −24, ana −36 → ranks bo, cy, ana.
	body := ana.get(fmt.Sprintf("/groups/%d", g)).Body
	for _, want := range []string{
		`<span class="rank">1ST</span>`, `bo</span>`, `+60.00</span>`, "▲ is owed USD",
		`<span class="rank">2ND</span>`, `cy</span>`, `−24.00</span>`, "▼ owes USD",
		`<span class="rank">3RD</span>`, `ana</span>`, `−36.00</span>`,
	} {
		i := strings.Index(body, want)
		if i < 0 {
			t.Fatalf("high scores missing %q (or out of order)", want)
		}
		body = body[i+len(want):] // each must come after the previous one
	}
}

func TestYourBalanceCard(t *testing.T) {
	e := newEnv(t)
	ana, bo, cy, di := e.login("ana"), e.login("bo"), e.login("cy"), e.login("di")
	g := ana.createGroup("Vegas 2026", "USD")
	tok := ana.invite(g)
	bo.join(tok)
	cy.join(tok)
	di.join(tok)
	// Same as TestHighScoreBoard: bo +60, cy −24, ana −36, di 0. Settle up
	// suggests ana → bo 36 and cy → bo 24.
	bo.addEqualExpense(g, "Buffet", "90", bo.ID, ana.ID, bo.ID, cy.ID)
	cy.addEqualExpense(g, "Cab", "12", cy.ID, ana.ID, cy.ID)
	path := fmt.Sprintf("/groups/%d", g)

	card := func(c *client) string {
		t.Helper()
		body := c.get(path).Body
		i, j := strings.Index(body, `class="card my-balance"`), strings.Index(body, `class="card scroll"`)
		if i < 0 || j < 0 || i > j {
			t.Fatalf("your balance card missing or not above the expenses:\n%s", body)
		}
		return body[i:j]
	}
	inOrder := func(who, got string, want ...string) {
		t.Helper()
		for _, w := range want {
			k := strings.Index(got, w)
			if k < 0 {
				t.Fatalf("%s's card missing %q (or out of order):\n%s", who, w, got)
			}
			got = got[k+len(w):]
		}
	}

	c := card(ana)
	// What ana owes sits on the heading line too, after "Your balance".
	inOrder("ana", c, "Your balance</h2>", `<span aria-hidden="true">▼</span><span class="sr-only">You owe</span> 36.00`+"\u00a0USD</span>", "</div>", "Pay <strong>bo</strong> 36.00\u00a0USD", "Mark paid")
	if strings.Contains(c, "borrowed") {
		t.Error("ana is owed nothing but the card lists borrowers")
	}

	c = card(bo)
	// What bo is owed sits on the heading line, after "Your balance".
	inOrder("bo", c, "Your balance</h2>", `<span aria-hidden="true">▲</span><span class="sr-only">You're owed</span> 60.00`+"\u00a0USD</span>", "</div>",
		"<strong>ana</strong> borrowed 36.00\u00a0USD", "Mark paid",
		"<strong>cy</strong> borrowed 24.00\u00a0USD", "Mark paid")
	if strings.Contains(c, "Pay <strong>") {
		t.Error("bo owes nothing but the card lists payments to make")
	}

	settled := func(who, c string) {
		t.Helper()
		for _, s := range []string{"▲", "▼", "Pay <strong>", "borrowed", "all square"} {
			if strings.Contains(c, s) {
				t.Errorf("%s is settled, so the card should be empty below the heading, but has %q:\n%s", who, s, c)
			}
		}
	}
	settled("di", card(di))

	// Payments, Members and Owner settings are collapsible and start closed.
	body := ana.get(path).Body
	if n := strings.Count(body, `<details class="fold">`); n != 3 {
		t.Errorf("want Payments, Members and Owner settings as 3 closed fold cards, got %d", n)
	}
	if !strings.Contains(body, `<summary><h2><img class="icon" src="/static/icons/coin.svg" alt="">Payments`) {
		t.Error("Payments heading is not the fold's toggle")
	}
	if body = bo.get(path).Body; strings.Count(body, `<details class="fold">`) != 2 || strings.Contains(body, "Owner settings") {
		t.Error("a non-owner should see only the Payments and Members folds")
	}

	// Marking your payment from the card settles you up.
	ana.mustPost(path+"/settlements", url.Values{"from": {itoa(ana.ID)}, "to": {itoa(bo.ID)}, "amount": {"36.00"}})
	settled("ana", card(ana))
}

func TestTimeFmt(t *testing.T) {
	at := time.Date(1986, 11, 18, 15, 7, 0, 0, time.UTC)
	for _, c := range []struct {
		f                timeFmt
		date, day, stamp string
	}{
		{timeFmt{"us", "12h"}, "11/18/1986", "07/01/2026", "11/18/1986 3:07 PM UTC"},
		{timeFmt{"us", "24h"}, "11/18/1986", "07/01/2026", "11/18/1986 15:07 UTC"},
		{timeFmt{"iso", "12h"}, "1986-11-18", "2026-07-01", "1986-11-18 3:07 PM UTC"},
		{timeFmt{"iso", "24h"}, "1986-11-18", "2026-07-01", "1986-11-18 15:07 UTC"},
	} {
		if got := c.f.Date(at); got != c.date {
			t.Errorf("%v Date = %q, want %q", c.f, got, c.date)
		}
		if got := c.f.Day("2026-07-01"); got != c.day {
			t.Errorf("%v Day = %q, want %q", c.f, got, c.day)
		}
		if got := c.f.Stamp(at); got != c.stamp {
			t.Errorf("%v Stamp = %q, want %q", c.f, got, c.stamp)
		}
	}
	if got := (timeFmt{"us", "12h"}).Day("not a date"); got != "not a date" {
		t.Errorf("bad Day input changed to %q", got)
	}
}

func TestDatesFollowPrefs(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	g := ana.createGroup("Lisbon", "EUR")
	ana.addEqualExpense(g, "Dinner", "10", ana.ID, ana.ID) // spent on 2026-07-01
	path, hist := fmt.Sprintf("/groups/%d", g), fmt.Sprintf("/groups/%d/history", g)
	stamp := regexp.MustCompile(`<td class="muted small nowrap">([^<]*)</td>`)

	// US dates and a 12-hour clock by default.
	if body := ana.get(path).Body; !strings.Contains(body, `<td class="nowrap">07/01/2026</td>`) {
		t.Error("expense date not in US format by default")
	}
	body := ana.get(hist).Body
	if !strings.Contains(body, "Dinner&#34;, 07/01/2026,") {
		t.Error("history expense date not in US format by default")
	}
	if m := stamp.FindStringSubmatch(body); m == nil || !regexp.MustCompile(`^\d\d/\d\d/\d{4} \d{1,2}:\d\d [AP]M UTC$`).MatchString(m[1]) {
		t.Errorf("history timestamp not US 12-hour: %v", m)
	}
	if body := ana.get("/groups").Body; !regexp.MustCompile(`created \d\d/\d\d/\d{4}`).MatchString(body) {
		t.Error("group list date not in US format")
	}

	ana.mustPost("/prefs", url.Values{"dates": {"iso"}, "clock": {"24h"}, "next": {path}})
	if body := ana.get(path).Body; !strings.Contains(body, `<td class="nowrap">2026-07-01</td>`) {
		t.Error("expense date not in ISO format after choosing ISO")
	}
	body = ana.get(hist).Body
	if !strings.Contains(body, "Dinner&#34;, 2026-07-01,") {
		t.Error("history expense date not in ISO format after choosing ISO")
	}
	if m := stamp.FindStringSubmatch(body); m == nil || !regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d UTC$`).MatchString(m[1]) {
		t.Errorf("history timestamp not ISO 24-hour: %v", m)
	}
}

func TestOrdinal(t *testing.T) {
	for n, want := range map[int]string{1: "1ST", 2: "2ND", 3: "3RD", 4: "4TH", 11: "11TH", 12: "12TH", 13: "13TH", 21: "21ST", 22: "22ND", 101: "101ST", 111: "111TH"} {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d) = %s, want %s", n, got, want)
		}
	}
}

func TestSplitTag(t *testing.T) {
	e := newEnv(t)
	ana := e.login("ana")
	g := ana.createGroup("Lisbon", "EUR")
	ana.addEqualExpense(g, "Dinner", "10", ana.ID, ana.ID)
	body := ana.get(fmt.Sprintf("/groups/%d", g)).Body
	if !strings.Contains(body, `<span class="split-tag">equal</span>`) {
		t.Errorf("equal split should show its name with no symbol:\n%s", body)
	}
	if strings.Contains(body, "split-sym") {
		t.Error("split type still has a symbol")
	}
}
