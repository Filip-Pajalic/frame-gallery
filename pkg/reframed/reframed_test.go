package reframed

import (
	"fmt"
	"testing"
	"time"
)

func TestTileParsing(t *testing.T) {
	page := `<h1 class="x">Winter</h1><a class="Tile-module__aQluCq__link" href="/carl-larsson/the-yard-and-washhouse"><div style="background-image:url(https://cdn.reframed.gallery/cdn-cgi/image/width=100/originals/x.jpg)"></div><img class="Tile-module__aQluCq__image " src="https://cdn.reframed.gallery/cdn-cgi/image/width=700,quality=80,format=auto/originals/Carl%20Larsson%20-%20The%20Yard%20and%20Washhouse%20-%20reframed.jpg" alt="The Yard and Washhouse" decoding="async"/></a>`
	m := tileRe.FindAllStringSubmatch(page, -1)
	if len(m) != 1 || m[0][1] != "/carl-larsson/the-yard-and-washhouse" || m[0][3] != "The Yard and Washhouse" {
		t.Fatalf("tiles %q", m)
	}
	if a := artistFromImage(m[0][2], m[0][1]); a != "Carl Larsson" {
		t.Fatalf("artist %q", a)
	}
}

func TestPaths(t *testing.T) {
	for p, ok := range map[string]bool{"/vincent-van-gogh/irises": true, "/a/b/c": false, "//evil.com/x": false, "/../etc": false, "/x/Y": false} {
		if pathOK(p) != ok {
			t.Errorf("pathOK(%q) != %v", p, ok)
		}
	}
}

func TestSeasons(t *testing.T) {
	cases := map[time.Month]string{time.January: "Winter", time.April: "Spring", time.July: "Summer", time.October: "Autumn", time.December: "December"}
	for m, want := range cases {
		if got := SeasonFor(time.Date(2026, m, 10, 0, 0, 0, 0, time.UTC)).Name; got != want {
			t.Errorf("%v: %s, want %s", m, got, want)
		}
	}
}

// The page cache stops growing at maxCached; values are still returned.
func TestCacheIsBounded(t *testing.T) {
	c := New()
	for i := 0; i < maxCached+20; i++ {
		v, err := c.remember(fmt.Sprint(i), func() (any, error) { return i, nil })
		if err != nil || v.(int) != i {
			t.Fatalf("remember %d: %v %v", i, v, err)
		}
	}
	if len(c.cache) > maxCached {
		t.Fatalf("cache holds %d entries, limit %d", len(c.cache), maxCached)
	}
}
