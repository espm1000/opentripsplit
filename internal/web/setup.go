package web

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/espm1000/opentripsplit/internal/auth"
	"github.com/espm1000/opentripsplit/internal/config"
	"github.com/espm1000/opentripsplit/internal/store"
)

// First-run setup works without any env vars:
//
//  1. While the instance has no admin, the server logs a random setup token.
//  2. Whoever has it opens /setup, enters it with OAuth credentials, and gets
//     a short-lived claim cookie.
//  3. Their next login (carrying the cookie) makes them the admin, who can
//     then change providers at /admin.
//
// Credentials from environment variables always win and are read-only in
// the UI.

const (
	setupCookie   = "ots_setup"
	setupClaimTTL = time.Hour
)

func settingKey(provider, field string) string { return "oauth." + provider + "." + field }

// providerState is one provider's effective configuration.
type providerState struct {
	Name, Label string
	EnvPrefix   string // e.g. GITHUB
	Source      string // "env", "ui" or "" (off)
	ClientID    string
	HasSecret   bool
	CallbackURL string
	creds       config.OAuthCreds
}

func (s *Server) providerStates(ctx context.Context) ([]providerState, error) {
	settings, err := s.store.Settings(ctx)
	if err != nil {
		return nil, err
	}
	var out []providerState
	for _, k := range auth.Known {
		ps := providerState{
			Name: k.Name, Label: k.Label, EnvPrefix: strings.ToUpper(k.Name),
			CallbackURL: s.baseURL.JoinPath("auth", k.Name, "callback").String(),
		}
		if c, ok := s.envProviders[k.Name]; ok {
			ps.Source, ps.creds = "env", c
		} else if id, sec := settings[settingKey(k.Name, "client_id")], settings[settingKey(k.Name, "client_secret")]; id != "" && sec != "" {
			ps.Source, ps.creds = "ui", config.OAuthCreds{ClientID: id, ClientSecret: sec}
		}
		ps.ClientID, ps.HasSecret = ps.creds.ClientID, ps.creds.ClientSecret != ""
		out = append(out, ps)
	}
	return out, nil
}

// reloadProviders rebuilds the enabled login providers from the environment
// and the database.
func (s *Server) reloadProviders(ctx context.Context) error {
	states, err := s.providerStates(ctx)
	if err != nil {
		return err
	}
	var ps []*auth.Provider
	for _, st := range states {
		if st.Source == "" {
			continue
		}
		p := auth.New(st.Name, st.creds.ClientID, st.creds.ClientSecret, st.CallbackURL)
		if s.wrapProvider != nil {
			s.wrapProvider(p)
		}
		ps = append(ps, p)
	}
	s.auth.SetProviders(ps)
	return nil
}

// currentSetupToken returns the setup token while the instance has no
// admin, creating and logging one if needed. It returns "" once an admin
// exists.
func (s *Server) currentSetupToken(ctx context.Context) (string, error) {
	hasAdmin, err := s.store.HasAdmin(ctx)
	if err != nil {
		return "", err
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if hasAdmin {
		s.setupToken = ""
		return "", nil
	}
	if s.setupToken == "" {
		s.setupToken = auth.NewToken()
		setupURL := s.baseURL.JoinPath("setup").String()
		if len(s.auth.Providers()) == 0 {
			s.log.Warn("no login provider configured: open the setup page and enter the setup token",
				"url", setupURL, "setup_token", s.setupToken)
		} else {
			s.log.Info("no admin yet: to manage login providers in the browser, open the setup page and enter the setup token",
				"url", setupURL, "setup_token", s.setupToken)
		}
	}
	return s.setupToken, nil
}

func (s *Server) announceSetup(ctx context.Context) error {
	_, err := s.currentSetupToken(ctx)
	return err
}

// claimAdmin runs after every login. If the browser completed /setup with
// the current setup token and there is still no admin, this user becomes
// the admin.
func (s *Server) claimAdmin(w http.ResponseWriter, r *http.Request, u store.User) {
	c, err := r.Cookie(setupCookie)
	if err != nil {
		return
	}
	http.SetCookie(w, s.auth.Cookie(setupCookie, "", "/", time.Time{}))
	s.setupMu.Lock()
	tok := s.setupToken
	s.setupMu.Unlock()
	if tok == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(tok)) != 1 {
		return
	}
	ok, err := s.store.ClaimAdmin(r.Context(), u.ID)
	if err != nil {
		s.log.Error("claim admin", "err", err)
		return
	}
	if ok {
		s.setupMu.Lock()
		s.setupToken = ""
		s.setupMu.Unlock()
		s.log.Info("instance admin set up", "user_id", u.ID)
	}
}

type setupView struct {
	base
	Providers []providerState
	Error     string
	Saved     bool
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, page string, status int, errMsg string) {
	states, err := s.providerStates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	title := "Set up OpenTripSplit"
	if page == "admin" {
		title = "Admin"
	}
	s.render(w, status, page, setupView{
		base: s.base(r, title), Providers: states, Error: errMsg, Saved: r.URL.Query().Get("saved") == "1",
	})
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	tok, err := s.currentSetupToken(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if tok == "" {
		s.errorPage(w, r, http.StatusNotFound, "Setup is already complete. The instance admin can change settings on the Admin page.")
		return
	}
	s.renderSetup(w, r, "setup", http.StatusOK, "")
}

func (s *Server) saveSetup(w http.ResponseWriter, r *http.Request) {
	tok, err := s.currentSetupToken(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if tok == "" {
		s.errorPage(w, r, http.StatusNotFound, "Setup is already complete.")
		return
	}
	given := strings.TrimSpace(r.PostFormValue("setup_token"))
	if subtle.ConstantTimeCompare([]byte(given), []byte(tok)) != 1 {
		s.renderSetup(w, r, "setup", http.StatusForbidden, "That setup token is wrong. Copy it from the server log.")
		return
	}
	if !s.saveProviders(w, r, "setup") {
		return
	}
	http.SetCookie(w, s.auth.Cookie(setupCookie, tok, "/", time.Now().Add(setupClaimTTL)))
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// admin wraps a handler for the instance admin. Everyone else gets 404.
func (s *Server) admin(h func(http.ResponseWriter, *http.Request, store.User)) http.Handler {
	return s.user(func(w http.ResponseWriter, r *http.Request, u store.User) {
		if !u.IsAdmin {
			s.errorPage(w, r, http.StatusNotFound, "Page not found.")
			return
		}
		h(w, r, u)
	})
}

func (s *Server) adminPage(w http.ResponseWriter, r *http.Request, _ store.User) {
	s.renderSetup(w, r, "admin", http.StatusOK, "")
}

func (s *Server) saveAdminProviders(w http.ResponseWriter, r *http.Request, _ store.User) {
	if s.saveProviders(w, r, "admin") {
		http.Redirect(w, r, "/admin?saved=1", http.StatusSeeOther)
	}
}

// saveProviders stores the provider credentials from the form and reloads
// the providers. On a validation error it re-renders page and returns false.
// A blank secret keeps the stored one; a blank client ID turns the provider
// off. Providers set by environment variables are left alone.
func (s *Server) saveProviders(w http.ResponseWriter, r *http.Request, page string) bool {
	ctx := r.Context()
	settings, err := s.store.Settings(ctx)
	if err != nil {
		s.fail(w, r, err)
		return false
	}
	set := map[string]string{}
	var del []string
	enabled := 0
	for _, k := range auth.Known {
		if _, ok := s.envProviders[k.Name]; ok {
			enabled++
			continue
		}
		idKey, secKey := settingKey(k.Name, "client_id"), settingKey(k.Name, "client_secret")
		id := strings.TrimSpace(r.PostFormValue(k.Name + "_client_id"))
		sec := strings.TrimSpace(r.PostFormValue(k.Name + "_client_secret"))
		if id == "" {
			del = append(del, idKey, secKey)
			continue
		}
		if sec == "" {
			sec = settings[secKey]
		}
		if sec == "" {
			s.renderSetup(w, r, page, http.StatusBadRequest, "Enter the "+k.Label+" client secret.")
			return false
		}
		set[idKey], set[secKey] = id, sec
		enabled++
	}
	if enabled == 0 {
		s.renderSetup(w, r, page, http.StatusBadRequest, "Configure at least one login provider, or nobody will be able to log in.")
		return false
	}
	if err := s.store.UpdateSettings(ctx, set, del); err != nil {
		s.fail(w, r, err)
		return false
	}
	if err := s.reloadProviders(ctx); err != nil {
		s.fail(w, r, err)
		return false
	}
	return true
}
