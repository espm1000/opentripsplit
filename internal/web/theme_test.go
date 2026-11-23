package web

import (
	"fmt"
	"maps"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The theme is tuned for keratoconus (see the comment at the top of
// app.css). These tests keep later colour tweaks from breaking that.

var tokenRe = regexp.MustCompile(`--([a-z-]+):\s*(#[0-9A-Fa-f]{6})\s*;`)

// palette returns the colour tokens declared in the CSS block that follows
// selector.
func palette(t *testing.T, css, selector string) map[string]string {
	t.Helper()
	i := strings.Index(css, selector)
	if i < 0 {
		t.Fatalf("selector %q not found in app.css", selector)
	}
	block := css[i:]
	block = block[strings.Index(block, "{")+1 : strings.Index(block, "}")]
	out := map[string]string{}
	for _, m := range tokenRe.FindAllStringSubmatch(block, -1) {
		out[m[1]] = strings.ToUpper(m[2])
	}
	if len(out) == 0 {
		t.Fatalf("no colour tokens under %q", selector)
	}
	return out
}

func luminance(hex string) float64 {
	var c [3]float64
	for i := range c {
		v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
		x := float64(v) / 255
		if x <= 0.03928 {
			c[i] = x / 12.92
		} else {
			c[i] = math.Pow((x+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

func contrast(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func appCSS(t *testing.T) string {
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPalettesMeetKeratoconusTargets(t *testing.T) {
	css := appCSS(t)
	light := palette(t, css, ":root {\n  color-scheme: light;")
	darkMedia := palette(t, css, `:root:not([data-theme="light"])`)
	dark := palette(t, css, `:root[data-theme="dark"] {`)
	if !maps.Equal(dark, darkMedia) {
		t.Fatalf("the two night palette blocks differ:\n%v\n%v", dark, darkMedia)
	}

	for name, p := range map[string]map[string]string{"parchment": light, "night": dark} {
		check := func(fg, bg string, min float64) {
			t.Helper()
			r := contrast(p[fg], p[bg])
			if r < min {
				t.Errorf("%s: --%s on --%s is %.2f:1, want >= %.1f:1", name, fg, bg, r, min)
			}
		}
		// Every text colour is AAA (7:1) on every background it can sit on.
		for _, bg := range []string{"bg", "surface", "notice", "chip"} {
			for _, fg := range []string{"text", "muted", "pos", "neg", "danger", "link", "accent"} {
				check(fg, bg, 7)
			}
			check("border", bg, 3)
			check("focus", bg, 3)
		}
		check("on-accent", "accent", 7)
		check("on-danger", "danger", 7)
		check("on-link", "link", 7)
		// The simulator's terminal has its own dark background.
		for _, fg := range []string{"term-text", "term-dim", "term-prompt", "term-ok", "term-fail"} {
			check(fg, "term-bg", 7)
		}

		// But never maximal: pure black/white and 21:1 cause halos.
		for _, tok := range []string{"bg", "surface", "text"} {
			if p[tok] == "#FFFFFF" || p[tok] == "#000000" {
				t.Errorf("%s: --%s is pure %s", name, tok, p[tok])
			}
		}
		if r := contrast(p["text"], p["bg"]); r > 15 {
			t.Errorf("%s: body text contrast %.1f:1 is harsher than needed (max 15:1)", name, r)
		}
	}
}

func TestNoPureBlackOrWhiteAnywhere(t *testing.T) {
	// Colour values only; comments are stripped (the header mentions these words).
	re := regexp.MustCompile(`(?i):[^;{}]*(#fff\b|#ffffff\b|#000\b|#000000\b|\bwhite\b|\bblack\b)`)
	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(appCSS(t), "")
	if m := re.FindString(css); m != "" {
		t.Errorf("app.css uses pure black or white: %s", m)
	}
}

func TestStaticAssetTypes(t *testing.T) {
	e := newEnv(t)
	c := e.anon()
	for path, want := range map[string]string{
		"/static/fonts/pixelify-sans-latin-700-normal.woff2":              "font/woff2",
		"/static/fonts/atkinson-hyperlegible-next-latin-400-normal.woff2": "font/woff2",
		"/static/icons/logo.svg":                                          "image/svg+xml",
		"/static/app.css":                                                 "text/css",
	} {
		r := c.get(path)
		if r.Status != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), want) {
			t.Errorf("%s: %d %s, want %s", path, r.Status, r.Header.Get("Content-Type"), want)
		}
	}
	// Every font and icon the CSS and templates reference exists.
	for _, m := range regexp.MustCompile(`url\("([^"]+)"\)`).FindAllStringSubmatch(appCSS(t), -1) {
		if r := c.get("/static/" + m[1]); r.Status != 200 {
			t.Errorf("app.css references missing %s", m[1])
		}
	}
	for _, icon := range []string{"logo", "coin", "heart", "scroll", "key"} {
		if r := c.get(fmt.Sprintf("/static/icons/%s.svg", icon)); r.Status != 200 {
			t.Errorf("missing icon %s", icon)
		}
	}
}
