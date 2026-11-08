// Package auth implements login with OAuth2 providers (GitHub, Discord),
// cookie sessions, and the middleware that puts the current user in the
// request context.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/endpoints"
)

// ProviderUser is the minimum we keep from a provider profile.
type ProviderUser struct {
	ID   string
	Name string
}

// Provider is an OAuth2 login provider. Fields are exported so tests can
// point a provider at a fake server.
type Provider struct {
	Name    string // URL slug and identities.provider value
	Label   string // button text
	Config  *oauth2.Config
	UserURL string
	Parse   func([]byte) (ProviderUser, error)
}

// Known lists the supported providers, in display order.
var Known = []struct{ Name, Label string }{
	{"github", "GitHub"},
	{"discord", "Discord"},
}

// New returns the named provider, or nil if the name is unknown.
func New(name, clientID, clientSecret, redirectURL string) *Provider {
	switch name {
	case "github":
		return GitHub(clientID, clientSecret, redirectURL)
	case "discord":
		return Discord(clientID, clientSecret, redirectURL)
	}
	return nil
}

// GitHub requests no scopes: the public profile is enough, and we keep only
// the numeric ID and login.
func GitHub(clientID, clientSecret, redirectURL string) *Provider {
	return &Provider{
		Name:  "github",
		Label: "GitHub",
		Config: &oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: endpoints.GitHub,
		},
		UserURL: "https://api.github.com/user",
		Parse:   parseGitHub,
	}
}

func parseGitHub(b []byte) (ProviderUser, error) {
	var v struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ProviderUser{}, err
	}
	if v.ID == 0 {
		return ProviderUser{}, fmt.Errorf("github: profile has no id")
	}
	return ProviderUser{ID: strconv.FormatInt(v.ID, 10), Name: v.Login}, nil
}

// Discord requests only the identify scope (no email).
func Discord(clientID, clientSecret, redirectURL string) *Provider {
	return &Provider{
		Name:  "discord",
		Label: "Discord",
		Config: &oauth2.Config{
			ClientID: clientID, ClientSecret: clientSecret, RedirectURL: redirectURL,
			Endpoint: endpoints.Discord,
			Scopes:   []string{"identify"},
		},
		UserURL: "https://discord.com/api/users/@me",
		Parse:   parseDiscord,
	}
}

func parseDiscord(b []byte) (ProviderUser, error) {
	var v struct {
		ID         string  `json:"id"`
		Username   string  `json:"username"`
		GlobalName *string `json:"global_name"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return ProviderUser{}, err
	}
	if v.ID == "" {
		return ProviderUser{}, fmt.Errorf("discord: profile has no id")
	}
	name := v.Username
	if v.GlobalName != nil && *v.GlobalName != "" {
		name = *v.GlobalName
	}
	return ProviderUser{ID: v.ID, Name: name}, nil
}

// fetchUser exchanges the code (with the PKCE verifier) for a token, reads
// the profile, and drops the token.
func (p *Provider) fetchUser(ctx context.Context, code, verifier string) (ProviderUser, error) {
	tok, err := p.Config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return ProviderUser{}, fmt.Errorf("%s: token exchange: %w", p.Name, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserURL, nil)
	if err != nil {
		return ProviderUser{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.Config.Client(ctx, tok).Do(req)
	if err != nil {
		return ProviderUser{}, fmt.Errorf("%s: profile: %w", p.Name, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ProviderUser{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return ProviderUser{}, fmt.Errorf("%s: profile: status %d", p.Name, resp.StatusCode)
	}
	return p.Parse(body)
}
