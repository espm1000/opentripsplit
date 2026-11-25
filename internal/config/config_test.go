package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"GITHUB_CLIENT_ID": "id", "GITHUB_CLIENT_SECRET": "s"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.ListenAddr != ":8080" || c.DBPath != "opentripsplit.db" || c.SessionTTL != 720*time.Hour || c.InviteTTL != 168*time.Hour {
		t.Fatalf("defaults = %+v", c)
	}
	if c.Secure() {
		t.Error("http base URL should not set Secure cookies")
	}
	if got := c.CallbackURL("github"); got != "http://localhost:8080/auth/github/callback" {
		t.Errorf("callback = %s", got)
	}
}

func TestLoadHTTPS(t *testing.T) {
	c, err := Load(env(map[string]string{
		"BASE_URL": "https://split.example.com/", "DISCORD_CLIENT_ID": "id", "DISCORD_CLIENT_SECRET": "s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Secure() || c.CallbackURL("discord") != "https://split.example.com/auth/discord/callback" {
		t.Fatalf("config = %+v", c)
	}
}

func TestLoadWithoutProviders(t *testing.T) {
	c, err := Load(env(map[string]string{}))
	if err != nil {
		t.Fatalf("no providers should be allowed (set up in the UI): %v", err)
	}
	if len(c.EnvProviders()) != 0 {
		t.Fatalf("env providers = %v", c.EnvProviders())
	}
	c, _ = Load(env(map[string]string{"DISCORD_CLIENT_ID": "id", "DISCORD_CLIENT_SECRET": "s"}))
	if got := c.EnvProviders(); len(got) != 1 || got["discord"] != (OAuthCreds{"id", "s"}) {
		t.Fatalf("env providers = %v", got)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := map[string]map[string]string{
		"half github":       {"GITHUB_CLIENT_ID": "id"},
		"relative base url": {"BASE_URL": "split.example.com", "GITHUB_CLIENT_ID": "id", "GITHUB_CLIENT_SECRET": "s"},
		"bad ttl":           {"SESSION_TTL": "forever", "GITHUB_CLIENT_ID": "id", "GITHUB_CLIENT_SECRET": "s"},
		"cert without key":  {"TLS_CERT_FILE": "/etc/tls/server.crt"},
		"key without cert":  {"TLS_KEY_FILE": "/etc/tls/server.key"},
		"tls with http url": {"TLS_CERT_FILE": "/etc/tls/server.crt", "TLS_KEY_FILE": "/etc/tls/server.key"},
	}
	for name, m := range tests {
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
	_, err := Load(env(map[string]string{"GITHUB_CLIENT_ID": "id", "SESSION_TTL": "-1h"}))
	if err == nil || !strings.Contains(err.Error(), "GITHUB_CLIENT_SECRET") || !strings.Contains(err.Error(), "SESSION_TTL") {
		t.Errorf("errors should be reported together: %v", err)
	}
}

func TestDevLocalAuth(t *testing.T) {
	ok := []string{"http://localhost:8080", "http://127.0.0.1:9000", "http://[::1]:8080"}
	for _, base := range ok {
		c, err := Load(env(map[string]string{"BASE_URL": base, "DEV_LOCAL_AUTH": "true"}))
		if err != nil || !c.DevLocalAuth {
			t.Errorf("%s: DevLocalAuth = %v, err = %v", base, c.DevLocalAuth, err)
		}
	}
	refused := []string{"https://split.example.com", "http://192.168.1.10:8080", "http://localhost.example.com"}
	for _, base := range refused {
		if _, err := Load(env(map[string]string{"BASE_URL": base, "DEV_LOCAL_AUTH": "1"})); err == nil {
			t.Errorf("%s: dev login allowed on a non-local BASE_URL", base)
		}
	}
	if c, _ := Load(env(map[string]string{})); c.DevLocalAuth {
		t.Error("dev login on by default")
	}
	if _, err := Load(env(map[string]string{"DEV_LOCAL_AUTH": "maybe"})); err == nil {
		t.Error("bad DEV_LOCAL_AUTH value accepted")
	}
}

func TestDebug(t *testing.T) {
	if c, _ := Load(env(map[string]string{})); c.Debug {
		t.Error("debug on by default")
	}
	if c, err := Load(env(map[string]string{"DEBUG": "true"})); err != nil || !c.Debug {
		t.Errorf("DEBUG=true: Debug = %v, err = %v", c.Debug, err)
	}
	if _, err := Load(env(map[string]string{"DEBUG": "verbose"})); err == nil {
		t.Error("bad DEBUG value accepted")
	}
}

func TestSimulator(t *testing.T) {
	if c, _ := Load(env(map[string]string{})); c.Simulator {
		t.Error("simulator on by default")
	}
	for _, v := range []string{"1", "true", "on"} {
		if c, err := Load(env(map[string]string{"SIMULATOR": v})); err != nil || !c.Simulator {
			t.Errorf("SIMULATOR=%s: Simulator = %v, err = %v", v, c.Simulator, err)
		}
	}
	// Unlike dev login it is allowed on a public BASE_URL: it is a demo.
	if c, err := Load(env(map[string]string{"BASE_URL": "https://split.example.com", "SIMULATOR": "yes"})); err != nil || !c.Simulator {
		t.Errorf("public BASE_URL: Simulator = %v, err = %v", c.Simulator, err)
	}
	if _, err := Load(env(map[string]string{"SIMULATOR": "maybe"})); err == nil {
		t.Error("bad SIMULATOR value accepted")
	}
}

func TestTLS(t *testing.T) {
	if c, _ := Load(env(map[string]string{})); c.TLS() {
		t.Error("TLS on by default")
	}
	c, err := Load(env(map[string]string{
		"BASE_URL": "https://split.example.com", "TLS_CERT_FILE": "/etc/tls/server.crt", "TLS_KEY_FILE": "/etc/tls/server.key",
	}))
	if err != nil || !c.TLS() || c.TLSCertFile != "/etc/tls/server.crt" || c.TLSKeyFile != "/etc/tls/server.key" {
		t.Errorf("TLS = %v, config = %+v, err = %v", c.TLS(), c, err)
	}
}
