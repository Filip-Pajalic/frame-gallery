package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Filip-Pajalic/frame-gallery/internal/library"
	"github.com/Filip-Pajalic/frame-gallery/pkg/reframed"
)

// browsePage lists Reframed Gallery's art next to the library's own sidebar.
type browsePage struct {
	Filter      library.Filter
	All, OnTV   int
	Collections []library.Collection

	Season     reframed.Season
	SeasonList []reframed.Collection
	Sources    []reframed.Collection
	Current    string // collection slug on screen
	Heading    string
	Query      string
	Cards      []browseCard
	Error      string
	Browsing   bool
}

type browseCard struct {
	reframed.Card
	InLibrary int64 // library id when already imported
}

func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := browsePage{Season: reframed.SeasonFor(time.Now()), Query: strings.TrimSpace(r.URL.Query().Get("q")),
		Current: r.URL.Query().Get("c"), Browsing: true}
	var err error
	if p.Collections, p.All, p.OnTV, err = s.sideData(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	all, err := s.Ref.Collections(ctx)
	if err != nil {
		p.Error = err.Error()
		s.render(w, "browse.html", p)
		return
	}
	p.Sources = all
	byslug := map[string]reframed.Collection{}
	for _, c := range all {
		byslug[c.Slug] = c
	}
	for _, slug := range p.Season.Collections {
		if c, ok := byslug[slug]; ok {
			p.SeasonList = append(p.SeasonList, c)
		}
	}

	var cards []reframed.Card
	switch {
	case p.Query != "":
		p.Current = ""
		var total int
		cards, total, err = s.Ref.Search(ctx, p.Query, maxSearchResults)
		p.Heading = fmt.Sprintf("Search: %s", p.Query)
		if total > len(cards) {
			p.Heading += fmt.Sprintf(" (first %d of %d; add a word to narrow it)", len(cards), total)
		}
	default:
		if p.Current == "" && len(p.SeasonList) > 0 {
			p.Current = p.SeasonList[0].Slug
		}
		if p.Current != "" {
			p.Heading, cards, err = s.Ref.CollectionCards(ctx, p.Current)
		}
	}
	if err != nil {
		p.Error = err.Error()
	}
	have, herr := s.importedFromReframed()
	if herr != nil {
		http.Error(w, herr.Error(), 500)
		return
	}
	for _, c := range cards {
		p.Cards = append(p.Cards, browseCard{Card: c, InLibrary: have[reframed.Base+c.Path]})
	}
	s.render(w, "browse.html", p)
}

// maxSearchResults is how many search hits one page shows; the heading says
// when there are more.
const maxSearchResults = 60

// importedFromReframed maps source pages to library ids.
func (s *Server) importedFromReframed() (map[string]int64, error) {
	items, err := s.Lib.List(library.Filter{})
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, it := range items {
		if strings.HasPrefix(it.URL, reframed.Base+"/") {
			out[it.URL] = it.ID
		}
	}
	return out, nil
}

// browseThumb sends the browser to a work's thumbnail (search results know
// only the page, not the picture).
func (s *Server) browseThumb(w http.ResponseWriter, r *http.Request) {
	wk, err := s.Ref.Work(r.Context(), r.URL.Query().Get("p"))
	if err != nil || wk.Thumb == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.Redirect(w, r, wk.Thumb, http.StatusFound)
}

func (s *Server) browseImport(w http.ResponseWriter, r *http.Request) {
	path := r.FormValue("path")
	coll, err := optionalID(r.FormValue("collection"))
	if err != nil {
		reply(w, 400, false, err.Error(), nil)
		return
	}
	have, err := s.importedFromReframed()
	if err != nil {
		reply(w, 500, false, err.Error(), nil)
		return
	}
	if id := have[reframed.Base+path]; id != 0 {
		reply(w, 200, true, "Already in the library", map[string]any{"id": id})
		return
	}
	wk, err := s.Ref.Work(r.Context(), path)
	if err != nil {
		reply(w, 502, false, err.Error(), nil)
		return
	}
	data, err := s.Ref.Download(r.Context(), wk)
	if err != nil {
		reply(w, 502, false, err.Error(), nil)
		return
	}
	it, err := s.Lib.Import(data, wk.Title+".jpg", library.Meta{Title: wk.Title, Artist: wk.Artist,
		Source: "Reframed Gallery", URL: reframed.Base + wk.Path})
	if err != nil {
		reply(w, 422, false, err.Error(), nil)
		return
	}
	msg := "In the library now"
	if it.Loss > 0.005 {
		msg += fmt.Sprintf(" (cropped %.0f %%)", it.Loss*100)
	}
	if coll != 0 {
		if err := s.Lib.SetInCollection(it.ID, coll, true); err != nil {
			msg += "; not added to the collection: " + err.Error()
		}
	}
	reply(w, 200, true, msg, map[string]any{"id": it.ID})
}
