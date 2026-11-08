// Package web holds the HTTP handlers, templates and static files.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/espm1000/opentripsplit/internal/auth"
	"github.com/espm1000/opentripsplit/internal/config"
	"github.com/espm1000/opentripsplit/internal/ledger"
	"github.com/espm1000/opentripsplit/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Options configures a Server.
type Options struct {
	BaseURL   *url.URL
	InviteTTL time.Duration
	// EnvProviders are OAuth credentials from environment variables. They
	// take precedence over credentials saved in the UI.
	EnvProviders map[string]config.OAuthCreds
	// DevLocalAuth turns on password-less dev login (see devauth.go).
	DevLocalAuth bool
	Log          *slog.Logger
}

// Server serves the web UI.
type Server struct {
	store        *store.Store
	auth         *auth.Auth
	baseURL      *url.URL
	inviteTTL    time.Duration
	envProviders map[string]config.OAuthCreds
	devAuth      bool
	log          *slog.Logger
	pages        map[string]*template.Template

	// wrapProvider lets tests point providers at a fake OAuth server.
	wrapProvider func(*auth.Provider)

	setupMu    sync.Mutex
	setupToken string // see setup.go
}

// New parses the templates, wires the auth hooks, and loads the login
// providers from the environment and the database.
func New(ctx context.Context, st *store.Store, a *auth.Auth, o Options) (*Server, error) {
	s := &Server{
		store: st, auth: a, baseURL: o.BaseURL, inviteTTL: o.InviteTTL,
		envProviders: o.EnvProviders, devAuth: o.DevLocalAuth, log: o.Log, pages: map[string]*template.Template{},
	}
	funcs := template.FuncMap{
		"date":     func(t time.Time) string { return t.Format("2 Jan 2006") },
		"datetime": func(t time.Time) string { return t.Format("2 Jan 2006 15:04 MST") },
		"amount":   ledger.FormatAmount,
		"abs":      func(v int64) int64 { return max(v, -v) },
	}
	pages, err := fs.Glob(templatesFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/"), ".html")
		if name == "layout" || name == "partials" {
			continue
		}
		t, err := template.New(name).Funcs(funcs).ParseFS(templatesFS, "templates/layout.html", "templates/partials.html", p)
		if err != nil {
			return nil, err
		}
		s.pages[name] = t
	}
	a.Fail = s.errorPage
	a.OnLogin = s.claimAdmin
	if err := s.reloadProviders(ctx); err != nil {
		return nil, err
	}
	if s.devAuth {
		s.log.Warn("DEV_LOCAL_AUTH is on: anyone who can reach this server can log in as anyone. Never enable it in production.")
	}
	if err := s.announceSetup(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler returns the full HTTP handler with middleware applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(static)))
	mux.HandleFunc("GET /healthz", s.healthz)

	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /auth/{provider}/login", s.auth.Login)
	mux.HandleFunc("GET /auth/{provider}/callback", s.auth.Callback)
	mux.HandleFunc("POST /logout", s.auth.Logout)

	mux.Handle("GET /groups", s.user(s.groupsPage))
	mux.Handle("POST /groups", s.user(s.createGroup))
	for _, rt := range s.groupRoutes() {
		mux.Handle(rt.pattern, s.member(rt.h))
	}

	mux.Handle("GET /invite/{token}", s.user(s.invitePage))
	mux.Handle("POST /invite/{token}", s.user(s.acceptInvite))

	s.registerDevRoutes(mux)

	mux.HandleFunc("GET /setup", s.setupPage)
	mux.HandleFunc("POST /setup", s.saveSetup)
	mux.Handle("GET /admin", s.admin(s.adminPage))
	mux.Handle("POST /admin/providers", s.admin(s.saveAdminProviders))

	mux.Handle("GET /account", s.user(s.accountPage))
	mux.Handle("POST /account/name", s.user(s.renameAccount))
	mux.Handle("POST /account/delete", s.user(s.deleteAccount))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusNotFound, "Page not found.")
	})

	cop := http.NewCrossOriginProtection()
	// Trust our public origin too, in case a proxy rewrites Host.
	if err := cop.AddTrustedOrigin(s.baseURL.Scheme + "://" + s.baseURL.Host); err != nil {
		s.log.Warn("trusted origin", "err", err)
	}
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusForbidden, "Cross-origin request blocked.")
	}))

	return s.logRequests(securityHeaders(cop.Handler(s.auth.LoadUser(mux))))
}

type groupRoute struct {
	pattern string
	h       groupHandler
}

// groupRoutes lists every /groups/{id}/... route. They all go through
// requireMember (see member), so non-members get 404.
func (s *Server) groupRoutes() []groupRoute {
	return []groupRoute{
		{"GET /groups/{id}", s.groupPage},
		{"GET /groups/{id}/history", s.historyPage},
		{"POST /groups/{id}/rename", s.renameGroup},
		{"POST /groups/{id}/archive", s.archiveGroup},
		{"POST /groups/{id}/unarchive", s.unarchiveGroup},
		{"POST /groups/{id}/leave", s.leaveGroup},
		{"POST /groups/{id}/members/{uid}/remove", s.removeMember},
		{"POST /groups/{id}/members/{uid}/owner", s.transferOwnership},
		{"POST /groups/{id}/invites", s.createInvite},
		{"POST /groups/{id}/invites/{iid}/revoke", s.revokeInvite},
		{"GET /groups/{id}/expenses/new", s.newExpensePage},
		{"POST /groups/{id}/expenses", s.createExpense},
		{"GET /groups/{id}/expenses/{eid}/edit", s.editExpensePage},
		{"POST /groups/{id}/expenses/{eid}", s.updateExpense},
		{"POST /groups/{id}/expenses/{eid}/delete", s.deleteExpense},
		{"GET /groups/{id}/split-fields", s.splitFields},
		{"POST /groups/{id}/settlements", s.createSettlement},
		{"POST /groups/{id}/settlements/{sid}/delete", s.deleteSettlement},
	}
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// logRequests logs method, path, status and duration. It never logs IP
// addresses, and redacts invite tokens from paths.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		path := r.URL.Path
		if strings.HasPrefix(path, "/invite/") {
			path = "/invite/[token]"
		}
		s.log.Info("request", "method", r.Method, "path", path, "status", sw.status, "dur", time.Since(start).Round(time.Microsecond))
	})
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

// base is embedded in every page's template data.
type base struct {
	User    *store.User
	Title   string
	DevAuth bool // shows the dev-login banner
}

func (s *Server) base(r *http.Request, title string) base {
	b := base{Title: title, DevAuth: s.devAuth}
	if u, ok := auth.UserFrom(r.Context()); ok {
		b.User = &u
	}
	return b
}

func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	s.renderTemplate(w, status, page, "layout", data)
}

func (s *Server) renderTemplate(w http.ResponseWriter, status int, page, name string, data any) {
	var buf bytes.Buffer
	t, ok := s.pages[page]
	if !ok {
		s.log.Error("unknown page", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

type errorView struct {
	base
	Status  int
	Message string
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, status, "error", errorView{base: s.base(r, http.StatusText(status)), Status: status, Message: msg})
}

// fail maps a store error to an error page.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ue *store.UserError
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.errorPage(w, r, http.StatusNotFound, "Not found.")
	case errors.Is(err, store.ErrForbidden):
		s.errorPage(w, r, http.StatusForbidden, capitalize(err.Error())+".")
	case errors.Is(err, store.ErrArchived), errors.Is(err, store.ErrNonZeroBalance), errors.Is(err, store.ErrOwnerMustStay):
		s.errorPage(w, r, http.StatusConflict, capitalize(err.Error())+".")
	case errors.Is(err, store.ErrInviteExpired), errors.Is(err, store.ErrInviteRevoked):
		s.errorPage(w, r, http.StatusGone, capitalize(err.Error())+". Ask the group owner for a new one.")
	case errors.As(err, &ue):
		s.errorPage(w, r, http.StatusBadRequest, capitalize(ue.Msg)+".")
	default:
		s.log.Error("request failed", "err", err)
		s.errorPage(w, r, http.StatusInternalServerError, "Something went wrong.")
	}
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// user wraps a handler that requires login. Anonymous visitors are sent to
// the login page and brought back afterwards.
func (s *Server) user(h func(http.ResponseWriter, *http.Request, store.User)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, ok := auth.UserFrom(r.Context())
		if !ok {
			next := ""
			if r.Method == http.MethodGet {
				next = r.URL.RequestURI()
			}
			http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		h(w, r, u)
	})
}

// groupCtx is the request context for /groups/{id}/... routes.
type groupCtx struct {
	User  store.User
	Group store.Group
	Me    store.Member
}

func (gc *groupCtx) IsOwner() bool { return gc.Me.Role == store.RoleOwner }

func (gc *groupCtx) path(parts ...string) string {
	return "/groups/" + strconv.FormatInt(gc.Group.ID, 10) + strings.Join(parts, "")
}

type groupHandler func(http.ResponseWriter, *http.Request, *groupCtx)

// member is the requireMember check: it loads the group and the user's
// active membership, and answers 404 (not 403) to anyone else so group IDs
// can't be probed.
func (s *Server) member(h groupHandler) http.Handler {
	return s.user(func(w http.ResponseWriter, r *http.Request, u store.User) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			s.errorPage(w, r, http.StatusNotFound, "Not found.")
			return
		}
		m, err := s.store.Membership(r.Context(), id, u.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		g, err := s.store.Group(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		h(w, r, &groupCtx{User: u, Group: g, Me: m})
	})
}

func pathID(r *http.Request, name string) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	return id, err == nil
}

func formID(r *http.Request, name string) int64 {
	id, _ := strconv.ParseInt(r.PostFormValue(name), 10, 64)
	return id
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserFrom(r.Context()); ok {
		http.Redirect(w, r, "/groups", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type loginView struct {
	base
	Providers []*auth.Provider
	Next      string
	DevUsers  []devUser
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	next := auth.SafeNext(r.URL.Query().Get("next"))
	// With dev login on, logged-in users may come back here to switch user.
	switching := s.devAuth && r.URL.Query().Get("switch") == "1"
	if _, ok := auth.UserFrom(r.Context()); ok && !switching {
		if next == "" {
			next = "/groups"
		}
		http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // next passed SafeNext (local paths only)
		return
	}
	v := loginView{base: s.base(r, "Log in"), Providers: s.auth.Providers(), Next: next}
	if s.devAuth {
		v.DevUsers = devUsers
	}
	s.render(w, http.StatusOK, "login", v)
}
