package auth

import "testing"

func TestParseGitHub(t *testing.T) {
	u, err := parseGitHub([]byte(`{"id": 583231, "login": "octocat", "email": "octo@example.com", "avatar_url": "x"}`))
	if err != nil || u != (ProviderUser{ID: "583231", Name: "octocat"}) {
		t.Fatalf("got %+v, %v", u, err)
	}
	if _, err := parseGitHub([]byte(`{"login": "x"}`)); err == nil {
		t.Fatal("missing id accepted")
	}
}

func TestParseDiscord(t *testing.T) {
	u, err := parseDiscord([]byte(`{"id": "80351110224678912", "username": "nelly", "global_name": "Nelly"}`))
	if err != nil || u != (ProviderUser{ID: "80351110224678912", Name: "Nelly"}) {
		t.Fatalf("got %+v, %v", u, err)
	}
	u, err = parseDiscord([]byte(`{"id": "1", "username": "nelly", "global_name": null}`))
	if err != nil || u.Name != "nelly" {
		t.Fatalf("null global_name: %+v, %v", u, err)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/groups/1":         "/groups/1",
		"/invite/abc":       "/invite/abc",
		"":                  "",
		"//evil.example":    "",
		"/\\evil.example":   "",
		"https://evil.test": "",
		"groups":            "",
	} {
		if got := SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestScopes(t *testing.T) {
	if s := GitHub("a", "b", "c").Config.Scopes; len(s) != 0 {
		t.Errorf("GitHub scopes = %v, want none", s)
	}
	if s := Discord("a", "b", "c").Config.Scopes; len(s) != 1 || s[0] != "identify" {
		t.Errorf("Discord scopes = %v, want [identify]", s)
	}
}
