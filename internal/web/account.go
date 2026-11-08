package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/espm1000/opentripsplit/internal/store"
)

type providerLink struct {
	Name, Label string
	Linked      bool
}

type accountView struct {
	base
	Providers []providerLink
	Error     string
}

func (s *Server) accountPage(w http.ResponseWriter, r *http.Request, u store.User) {
	s.renderAccount(w, r, u, http.StatusOK, "")
}

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, u store.User, status int, errMsg string) {
	ids, err := s.store.Identities(r.Context(), u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	linked := map[string]bool{}
	for _, id := range ids {
		linked[id.Provider] = true
	}
	v := accountView{base: s.base(r, "Account"), Error: errMsg}
	for _, p := range s.auth.Providers() {
		v.Providers = append(v.Providers, providerLink{p.Name, p.Label, linked[p.Name]})
	}
	s.render(w, status, "account", v)
}

func (s *Server) renameAccount(w http.ResponseWriter, r *http.Request, u store.User) {
	err := s.store.SetDisplayName(r.Context(), u.ID, r.PostFormValue("display_name"))
	var ue *store.UserError
	if errors.As(err, &ue) {
		s.renderAccount(w, r, u, http.StatusBadRequest, capitalize(ue.Msg)+".")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/account", http.StatusSeeOther)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, u store.User) {
	if strings.TrimSpace(r.PostFormValue("confirm")) != "DELETE" {
		s.renderAccount(w, r, u, http.StatusBadRequest, "Type DELETE to confirm.")
		return
	}
	if err := s.store.DeleteAccount(r.Context(), u.ID); err != nil {
		s.fail(w, r, err)
		return
	}
	// The store already deleted every session; clear the cookie too.
	s.auth.ClearSession(w)
	s.render(w, http.StatusOK, "deleted", base{Title: "Account deleted", DevAuth: s.devAuth})
}
