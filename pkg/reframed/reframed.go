// Package reframed reads the public pages of Reframed Gallery
// (https://www.reframed.gallery), a free collection of fine art already
// reframed to 3840x2160 for TVs, so the gallery can browse it and import one
// picture at a time. Nothing is fetched until someone opens the Browse page;
// pages are cached for a few hours and nothing is crawled in bulk.
package reframed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const Base = "https://www.reframed.gallery"

// Card is one artwork as a collection or search lists it.
type Card struct {
	Path   string // /artist/work, the page on reframed.gallery
	Title  string
	Artist string
	Thumb  string // their CDN, 700 px wide
}

// Work is one artwork page.
type Work struct {
	Card
	Image       string // the 3840x2160 original
	Description string
}

// Collection is a themed list on the site.
type Collection struct {
	Slug  string
	Title string
}

type Client struct {
	HTTP *http.Client
	TTL  time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	at  time.Time
	val any
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}, TTL: 6 * time.Hour, cache: map[string]cached{}}
}

func (c *Client) remember(key string, fn func() (any, error)) (any, error) {
	c.mu.Lock()
	if v, ok := c.cache[key]; ok && time.Since(v.at) < c.TTL {
		c.mu.Unlock()
		return v.val, nil
	}
	c.mu.Unlock()
	val, err := fn()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cache) >= maxCached {
		for k, v := range c.cache {
			if time.Since(v.at) >= c.TTL {
				delete(c.cache, k)
			}
		}
	}
	if len(c.cache) < maxCached {
		c.cache[key] = cached{time.Now(), val}
	}
	return val, nil
}

// maxCached bounds the page cache: a few hundred pages is a long browse.
// Past it, new pages are fetched each time instead of kept.
const maxCached = 500

func (c *Client) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "FrameGallery/1.0 (home use; one picture at a time)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("reframed.gallery not reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("reframed.gallery answered %s for %s", resp.Status, u)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("answer too large")
	}
	return b, nil
}

var locRe = regexp.MustCompile(`<loc>https://www\.reframed\.gallery(/[^<]*)</loc>`)

type index struct {
	collections []Collection
	works       []string // /artist/work
}

func (c *Client) index(ctx context.Context) (*index, error) {
	v, err := c.remember("sitemap", func() (any, error) {
		b, err := c.get(ctx, Base+"/sitemap.xml", 8<<20)
		if err != nil {
			return nil, err
		}
		ix := &index{}
		for _, m := range locRe.FindAllStringSubmatch(string(b), -1) {
			p := m[1]
			parts := strings.Split(strings.Trim(p, "/"), "/")
			switch {
			case len(parts) == 2 && parts[0] == "collections":
				ix.collections = append(ix.collections, Collection{Slug: parts[1], Title: Unslug(parts[1])})
			case len(parts) == 2 && parts[0] != "tags":
				ix.works = append(ix.works, p)
			}
		}
		sort.Slice(ix.collections, func(i, j int) bool { return ix.collections[i].Title < ix.collections[j].Title })
		return ix, nil
	})
	if err != nil {
		return nil, err
	}
	return v.(*index), nil
}

// Collections lists every collection on the site.
func (c *Client) Collections(ctx context.Context) ([]Collection, error) {
	ix, err := c.index(ctx)
	if err != nil {
		return nil, err
	}
	return ix.collections, nil
}

var (
	tileRe = regexp.MustCompile(`(?s)<a class="Tile[^"]*" href="(/[a-z0-9-]+/[a-z0-9-]+)">.*?<img [^>]*src="([^"]+)" alt="([^"]*)"`)
	h1Re   = regexp.MustCompile(`<h1[^>]*>([^<]+)</h1>`)
)

// CollectionCards lists the artworks of one collection.
func (c *Client) CollectionCards(ctx context.Context, slug string) (string, []Card, error) {
	if !slugOK(slug) {
		return "", nil, errors.New("no such collection")
	}
	type res struct {
		title string
		cards []Card
	}
	v, err := c.remember("c/"+slug, func() (any, error) {
		b, err := c.get(ctx, Base+"/collections/"+slug, 4<<20)
		if err != nil {
			return nil, err
		}
		r := res{title: Unslug(slug)}
		if m := h1Re.FindSubmatch(b); m != nil {
			r.title = html.UnescapeString(string(m[1]))
		}
		seen := map[string]bool{}
		for _, m := range tileRe.FindAllSubmatch(b, -1) {
			p := string(m[1])
			if seen[p] {
				continue
			}
			seen[p] = true
			thumb := html.UnescapeString(string(m[2]))
			r.cards = append(r.cards, Card{Path: p, Title: html.UnescapeString(string(m[3])),
				Artist: artistFromImage(thumb, p), Thumb: thumb})
		}
		return r, nil
	})
	if err != nil {
		return "", nil, err
	}
	r := v.(res)
	return r.title, r.cards, nil
}

// Search matches every word against the artist and title in the site's URLs
// and returns the first max hits with the total, so a caller can say there
// are more. Results carry no thumbnail; Work fills that in for the ones on screen.
func (c *Client) Search(ctx context.Context, q string, max int) ([]Card, int, error) {
	ix, err := c.index(ctx)
	if err != nil {
		return nil, 0, err
	}
	words := strings.Fields(strings.ToLower(Slugify(q)))
	if len(words) == 0 {
		return nil, 0, nil
	}
	total := 0
	var out []Card
	for _, p := range ix.works {
		hay := strings.ReplaceAll(p, "-", " ")
		ok := true
		for _, w := range words {
			if !strings.Contains(hay, w) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		total++
		if len(out) < max {
			parts := strings.Split(strings.Trim(p, "/"), "/")
			out = append(out, Card{Path: p, Artist: Unslug(parts[0]), Title: Unslug(parts[1])})
		}
	}
	return out, total, nil
}

var ldRe = regexp.MustCompile(`(?s)<script type="application/ld\+json">(.*?)</script>`)

// Work reads one artwork page.
func (c *Client) Work(ctx context.Context, path string) (Work, error) {
	if !pathOK(path) {
		return Work{}, errors.New("not an artwork page")
	}
	v, err := c.remember("w"+path, func() (any, error) {
		b, err := c.get(ctx, Base+path, 4<<20)
		if err != nil {
			return nil, err
		}
		for _, m := range ldRe.FindAllSubmatch(b, -1) {
			var ld struct {
				Type   string `json:"@type"`
				Name   string `json:"name"`
				Artist struct {
					Name string `json:"name"`
				} `json:"artist"`
				Content     string `json:"contentUrl"`
				Thumb       string `json:"thumbnailUrl"`
				Description string `json:"description"`
			}
			if json.Unmarshal(m[1], &ld) != nil || ld.Type != "VisualArtwork" || ld.Content == "" {
				continue
			}
			return Work{Card: Card{Path: path, Title: ld.Name, Artist: ld.Artist.Name, Thumb: ld.Thumb},
				Image: ld.Content, Description: ld.Description}, nil
		}
		return nil, errors.New("the page has no artwork data")
	})
	if err != nil {
		return Work{}, err
	}
	return v.(Work), nil
}

// Download fetches the full picture of one work.
func (c *Client) Download(ctx context.Context, w Work) ([]byte, error) {
	u, err := url.Parse(w.Image)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Host, "reframed.gallery") {
		return nil, errors.New("unexpected image address")
	}
	return c.get(ctx, w.Image, 80<<20)
}

// ---- seasons ----

// Season names what the gallery suggests for a date, with the site's
// collections that fit it.
type Season struct {
	Name        string
	Collections []string
}

// SeasonFor picks collections for the time of year (northern hemisphere).
func SeasonFor(t time.Time) Season {
	switch m := t.Month(); {
	case m == time.December:
		return Season{"December", []string{"christmas", "winter", "nocturnes-moonlight", "nordic-lights"}}
	case m <= time.February:
		return Season{"Winter", []string{"winter", "nordic-lights", "nocturnes-moonlight", "mountains-valleys"}}
	case m <= time.May:
		return Season{"Spring", []string{"spring-blossoms", "in-bloom", "the-english-countryside", "small-joys"}}
	case m <= time.August:
		return Season{"Summer", []string{"here-comes-the-sun", "by-the-sea", "joie-de-vivre", "waterways"}}
	default:
		return Season{"Autumn", []string{"fall", "into-the-woods", "golden-hour", "after-the-storm"}}
	}
}

// ---- helpers ----

var slugRe = regexp.MustCompile(`^[a-z0-9-]{1,120}$`)

func slugOK(s string) bool { return slugRe.MatchString(s) }

func pathOK(p string) bool {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	return strings.HasPrefix(p, "/") && len(parts) == 2 && slugOK(parts[0]) && slugOK(parts[1])
}

// Unslug turns "the-english-countryside" into "The english countryside".
func Unslug(s string) string {
	s = strings.ReplaceAll(s, "-", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Slugify keeps letters and digits, the way the site's URLs do.
func Slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune(' ')
		}
	}
	return b.String()
}

// artistFromImage reads "Carl Larsson" from ".../originals/Carl%20Larsson%20-%20Title%20-%20reframed.jpg".
func artistFromImage(img, path string) string {
	if i := strings.LastIndex(img, "/"); i >= 0 {
		if name, err := url.PathUnescape(img[i+1:]); err == nil {
			if j := strings.Index(name, " - "); j > 0 {
				return name[:j]
			}
		}
	}
	return Unslug(strings.Split(strings.Trim(path, "/"), "/")[0])
}
