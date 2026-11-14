// Package config reads settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

// Config is the server configuration.
type Config struct {
	BaseURL      *url.URL // public URL, e.g. https://split.example.com
	ListenAddr   string
	DBPath       string
	SessionTTL   time.Duration
	InviteTTL    time.Duration
	GitHubID     string
	GitHubSecret string
	DiscordID    string
	DiscordSec   string
	// DevLocalAuth enables password-less dev login (DEV_LOCAL_AUTH). It is
	// refused unless BASE_URL is localhost. That only checks BASE_URL: also
	// bind LISTEN_ADDR to loopback, since anyone who can reach the port can
	// log in.
	DevLocalAuth bool
}

// Secure reports whether cookies should carry the Secure flag.
func (c Config) Secure() bool { return c.BaseURL.Scheme == "https" }

// CallbackURL is the OAuth redirect URL to register with a provider.
func (c Config) CallbackURL(provider string) string {
	return c.BaseURL.JoinPath("auth", provider, "callback").String()
}

// Load reads the configuration using getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	c := Config{
		ListenAddr:   get("LISTEN_ADDR", ":8080"),
		DBPath:       get("DB_PATH", "opentripsplit.db"),
		GitHubID:     get("GITHUB_CLIENT_ID", ""),
		GitHubSecret: get("GITHUB_CLIENT_SECRET", ""),
		DiscordID:    get("DISCORD_CLIENT_ID", ""),
		DiscordSec:   get("DISCORD_CLIENT_SECRET", ""),
	}
	var errs []error

	u, err := url.Parse(get("BASE_URL", "http://localhost:8080"))
	switch {
	case err != nil:
		errs = append(errs, fmt.Errorf("BASE_URL: %w", err))
	case u.Scheme != "http" && u.Scheme != "https", u.Host == "":
		errs = append(errs, errors.New("BASE_URL must be an absolute http(s) URL"))
	default:
		u.Path = strings.TrimSuffix(u.Path, "/")
		c.BaseURL = u
	}

	duration := func(key, def string) time.Duration {
		d, err := time.ParseDuration(get(key, def))
		if err != nil || d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be a positive duration like %s", key, def))
		}
		return d
	}
	c.SessionTTL = duration("SESSION_TTL", "720h")
	c.InviteTTL = duration("INVITE_TTL", "168h")

	switch v := strings.ToLower(get("DEV_LOCAL_AUTH", "")); v {
	case "", "0", "false", "no", "off":
	case "1", "true", "yes", "on":
		c.DevLocalAuth = true
		if c.BaseURL != nil && !IsLoopbackHost(c.BaseURL.Hostname()) {
			errs = append(errs, errors.New("DEV_LOCAL_AUTH is only allowed when BASE_URL is localhost, 127.0.0.1 or [::1]; never enable it in production"))
		}
	default:
		errs = append(errs, fmt.Errorf("DEV_LOCAL_AUTH must be true or false, got %q", v))
	}

	if (c.GitHubID == "") != (c.GitHubSecret == "") {
		errs = append(errs, errors.New("set both GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET, or neither"))
	}
	if (c.DiscordID == "") != (c.DiscordSec == "") {
		errs = append(errs, errors.New("set both DISCORD_CLIENT_ID and DISCORD_CLIENT_SECRET, or neither"))
	}
	return c, errors.Join(errs...)
}

// EnvProviders returns the OAuth credentials set through environment
// variables, keyed by provider name. Providers not listed here can be
// configured in the UI.
func (c Config) EnvProviders() map[string]OAuthCreds {
	out := map[string]OAuthCreds{}
	if c.GitHubID != "" {
		out["github"] = OAuthCreds{c.GitHubID, c.GitHubSecret}
	}
	if c.DiscordID != "" {
		out["discord"] = OAuthCreds{c.DiscordID, c.DiscordSec}
	}
	return out
}

// OAuthCreds is an OAuth app's client ID and secret.
type OAuthCreds struct {
	ClientID     string
	ClientSecret string
}

// IsLoopbackHost reports whether host is localhost or a loopback IP.
func IsLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
