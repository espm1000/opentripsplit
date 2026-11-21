package web

import (
	"net/http"
	"strings"
	"testing"

	"github.com/espm1000/opentripsplit/internal/sim"
)

func newSimEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil, func(o *Options) { o.Simulator = true })
}

func TestSimulatorOffByDefault(t *testing.T) {
	c := newEnv(t).anon()
	if r := c.get("/simulator?seed=1"); r.Status != http.StatusNotFound {
		t.Fatalf("simulator answered %d with SIMULATOR off", r.Status)
	}
	if r := c.get("/login"); strings.Contains(r.Body, `href="/simulator"`) {
		t.Error("simulator link shown with SIMULATOR off")
	}
}

func TestSimulatorPublic(t *testing.T) {
	c := newSimEnv(t).anon()

	r := c.get("/simulator?people=3&expenses=6")
	if r.Status != http.StatusSeeOther || !strings.HasPrefix(r.Location, "/simulator?") ||
		!strings.Contains(r.Location, "seed=") || !strings.Contains(r.Location, "people=3") || !strings.Contains(r.Location, "expenses=6") {
		t.Fatalf("no seed: %d %q", r.Status, r.Location)
	}

	start := c.get("/simulator?seed=42")
	if start.Status != http.StatusOK || !strings.Contains(start.Body, "Step 0 of") {
		t.Fatalf("start page: %d\n%s", start.Status, start.Body)
	}
	if !strings.Contains(start.Body, `href="/simulator"`) || !strings.Contains(start.Body, "/static/animate.min.css") {
		t.Error("page lacks the nav link or the simulator stylesheet")
	}
	if strings.Contains(start.Body, "<script>") || strings.Contains(start.Body, "style=") {
		t.Error("inline script or style in page (blocked by CSP)")
	}
	if !strings.Contains(start.Header.Get("Content-Security-Policy"), "default-src 'self'") {
		t.Errorf("CSP = %q", start.Header.Get("Content-Security-Policy"))
	}

	// The end of every trip is all square, with no failed checks.
	end := c.get("/simulator?seed=42&at=9999")
	if end.Status != http.StatusOK || !strings.Contains(end.Body, "All square!") || strings.Contains(end.Body, "FAIL") {
		t.Fatalf("end page: %d\n%s", end.Status, end.Body)
	}
	if again := c.get("/simulator?seed=42&at=9999"); again.Body != end.Body {
		t.Error("same URL gave a different page")
	}

	// Other pages show the link too, logged out.
	if r := c.get("/login"); !strings.Contains(r.Body, `href="/simulator"`) {
		t.Error("login page lacks the simulator link")
	}
}

func TestSimulatorCurrency(t *testing.T) {
	c := newSimEnv(t).anon()
	r := c.get("/simulator?seed=5&currency=kwd&at=9999")
	if r.Status != http.StatusOK || !strings.Contains(r.Body, "/currency:KWD") || !strings.Contains(r.Body, `<option value="KWD" selected>`) {
		t.Fatalf("currency not used: %d", r.Status)
	}
	if !strings.Contains(r.Body, "All square!") || strings.Contains(r.Body, "FAIL") {
		t.Error("KWD trip didn't end square")
	}
	// New trip keeps the chosen currency.
	if !strings.Contains(r.Body, `href="/simulator?currency=KWD&amp;expenses=12&amp;people=4"`) {
		t.Error("New trip link drops the currency")
	}
	if r := c.get("/simulator?seed=5&currency=XYZ"); r.Status != http.StatusBadRequest {
		t.Errorf("unknown currency: %d, want 400", r.Status)
	}
}

func TestSimulatorBadParams(t *testing.T) {
	c := newSimEnv(t).anon()
	for _, q := range []string{"seed=abc", "seed=-1", "seed=1&people=x", "seed=1&at=1.5", "seed=1&currency=X"} {
		if r := c.get("/simulator?" + q); r.Status != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", q, r.Status)
		}
	}
	// Out-of-range sizes are clamped, not refused.
	r := c.get("/simulator?seed=1&people=500&expenses=0&at=-3")
	if r.Status != http.StatusOK || !strings.Contains(r.Body, "Step 0 of") {
		t.Fatalf("clamped: %d", r.Status)
	}
	if strings.Count(r.Body, `class="vt-p`) != sim.MaxPeople {
		t.Errorf("want %d players on the board", sim.MaxPeople)
	}
}

func TestSimulatorHTMXFragment(t *testing.T) {
	c := newSimEnv(t).anon()
	step := func(at, from string) string {
		t.Helper()
		r := c.do("GET", "/simulator?seed=7&people=4&expenses=8&at="+at, nil, map[string]string{
			"HX-Request":     "true",
			"HX-Current-URL": c.e.ts.URL + "/simulator?seed=7&people=4&expenses=8&at=" + from,
		})
		if r.Status != http.StatusOK || strings.Contains(r.Body, "<html") {
			t.Fatalf("at=%s: %d, want a fragment\n%s", at, r.Status, r.Body)
		}
		if r.Header.Get("Vary") != "HX-Request" {
			t.Errorf("Vary = %q", r.Header.Get("Vary"))
		}
		return r.Body
	}
	// One step forward appends one terminal block; a jump replaces the
	// terminal. Either way only the newest block is typed out, and the
	// board comes along out of band.
	b := step("3", "2")
	if !strings.Contains(b, `hx-swap-oob="beforeend:#sim-term"`) || strings.Count(b, "data-type") != 1 ||
		strings.Count(b, `class="terminal-block"`) != 1 || !strings.Contains(b, `id="sim-board" hx-swap-oob="true"`) {
		t.Errorf("single step should append one block:\n%s", b)
	}
	b = step("5", "2")
	if !strings.Contains(b, `id="sim-term" hx-swap-oob="innerHTML"`) || strings.Count(b, "data-type") != 1 ||
		strings.Count(b, `class="terminal-block"`) != 6 { // boot + 5 steps
		t.Errorf("jump should replace the terminal:\n%s", b)
	}

	// Autoplay keeps going until the end, then stops.
	r := c.do("GET", "/simulator?seed=7&people=4&expenses=8&at=1&play=1", nil, map[string]string{"HX-Request": "true"})
	if !strings.Contains(r.Body, `hx-trigger="load delay:`) || !strings.Contains(r.Body, "Pause") {
		t.Errorf("playing page has no autoplay trigger:\n%s", r.Body)
	}
	r = c.do("GET", "/simulator?seed=7&people=4&expenses=8&at=9999&play=1", nil, map[string]string{"HX-Request": "true"})
	if strings.Contains(r.Body, `hx-trigger="load delay:`) || !strings.Contains(r.Body, "Replay") {
		t.Errorf("finished trip still autoplays:\n%s", r.Body)
	}

	// A history restore wants the whole page.
	r = c.do("GET", "/simulator?seed=7&at=2", nil, map[string]string{"HX-Request": "true", "HX-History-Restore-Request": "true"})
	if !strings.Contains(r.Body, "<html") {
		t.Error("history restore got a fragment")
	}
}
