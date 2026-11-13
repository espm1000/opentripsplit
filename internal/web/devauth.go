package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/espm1000/opentripsplit/internal/auth"
	"github.com/espm1000/opentripsplit/internal/store"
)

// Dev login (DEV_LOCAL_AUTH) lets you log in as anyone without an OAuth
// app, for local testing. config.Load refuses it unless BASE_URL is
// localhost, and its routes aren't registered at all when it's off.

// devProvider is the identities.provider value for dev logins, so dev
// accounts never collide with real GitHub/Discord ones.
const devProvider = "dev"

// devUser is a ready-made test account.
type devUser struct {
	Key  string // provider user ID and form value
	Name string
}

// devUsers are the extra people to put in groups when testing.
var devUsers = []devUser{
	{"mike", "Mike (dev)"},
	{"chris", "Chris (dev)"},
	{"ryan", "Ryan (dev)"},
}

func (s *Server) registerDevRoutes(mux *http.ServeMux) {
	if !s.devAuth {
		return
	}
	mux.HandleFunc("POST /dev/login", s.devLogin)
	mux.Handle("POST /groups/{id}/dev/add-users", s.member(s.devAddUsers))
}

// devLogin logs in as a preset dev user (user=mike) or as anyone by name
// (name=Nick). Logging in while logged in switches user.
func (s *Server) devLogin(w http.ResponseWriter, r *http.Request) {
	var key, name string
	if k := r.PostFormValue("user"); k != "" {
		for _, du := range devUsers {
			if du.Key == k {
				key, name = du.Key, du.Name
			}
		}
	} else if n := strings.Join(strings.Fields(r.PostFormValue("name")), " "); n != "" {
		// The same name always reaches the same account.
		key, name = "name:"+strings.ToLower(n), n
	}
	if key == "" {
		s.errorPage(w, r, http.StatusBadRequest, "Pick a dev user or type a name.")
		return
	}
	u, err := s.store.LoginIdentity(r.Context(), devProvider, key, name)
	if err == nil {
		err = s.auth.StartSession(w, r, u.ID)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.claimAdmin(w, r, u)
	next := auth.SafeNext(r.PostFormValue("next"))
	if next == "" {
		next = "/groups"
	}
	http.Redirect(w, r, next, http.StatusSeeOther) //nolint:gosec // next passed SafeNext (local paths only)
}

// devAddUsers puts the preset dev users in the group. It goes through a
// one-off invite (created, used, revoked) so the normal permission checks
// and audit log apply: owner only, not on archived groups.
func (s *Server) devAddUsers(w http.ResponseWriter, r *http.Request, gc *groupCtx) {
	ctx := r.Context()
	token := auth.NewToken()
	inv, err := s.store.CreateInvite(ctx, gc.Group.ID, gc.User.ID, auth.HashToken(token), time.Minute)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for _, du := range devUsers {
		var u store.User
		if u, err = s.store.LoginIdentity(ctx, devProvider, du.Key, du.Name); err != nil {
			break
		}
		if _, err = s.store.AcceptInvite(ctx, auth.HashToken(token), u.ID); err != nil {
			break
		}
	}
	if rerr := s.store.RevokeInvite(ctx, gc.Group.ID, gc.User.ID, inv.ID); err == nil {
		err = rerr
	}
	s.done(w, r, gc, err)
}
