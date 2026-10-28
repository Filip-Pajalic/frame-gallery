// Package web is the gallery UI and its small JSON API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Filip-Pajalic/frame-gallery/internal/library"
	"github.com/Filip-Pajalic/frame-gallery/pkg/reframed"
	"github.com/Filip-Pajalic/frame-gallery/pkg/tv"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

type Server struct {
	Lib *library.Library
	TV  *tv.Client
	Ref *reframed.Client // Reframed Gallery, read only when someone browses it

	tpl *template.Template

	mu   sync.Mutex
	last TVState // what the TV said the last time someone asked; never refreshed on its own
}

// TVState is the last answer from the TV, kept only to show it.
type TVState struct {
	At        time.Time
	Reachable bool
	Message   string
	ArtMode   string
	Current   string
	API       string
}

func New(lib *library.Library, c *tv.Client) (*Server, error) {
	version := strconv.FormatInt(time.Now().Unix(), 36) // new asset URLs on every start
	funcs := template.FuncMap{
		"v":   func() string { return version },
		"pct": func(f float64) string { return fmt.Sprintf("%.0f %%", f*100) },
		"ago": ago,
		"has": func(list []int64, id int64) bool {
			for _, x := range list {
				if x == id {
					return true
				}
			}
			return false
		},
	}
	tpl, err := template.New("").Funcs(funcs).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{Lib: lib, TV: c, Ref: reframed.New(), tpl: tpl}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the directory is embedded at build time; this cannot fail at run time
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(http.FileServerFS(static), 3600)))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })

	mux.HandleFunc("GET /{$}", s.gallery)
	mux.HandleFunc("GET /item/{id}", s.itemPage)
	mux.HandleFunc("GET /img/{kind}/{id}", s.image)
	mux.HandleFunc("GET /browse", s.browse)
	mux.HandleFunc("GET /browse/thumb", s.browseThumb)
	mux.HandleFunc("POST /api/browse/import", s.browseImport)

	mux.HandleFunc("POST /api/import", s.importFiles)
	mux.HandleFunc("POST /api/item/{id}/meta", s.saveMeta)
	mux.HandleFunc("POST /api/item/{id}/show", s.show)
	mux.HandleFunc("POST /api/item/{id}/upload", s.upload)
	mux.HandleFunc("POST /api/item/{id}/remove-tv", s.removeFromTV)
	mux.HandleFunc("POST /api/item/{id}/delete", s.deleteItem)
	mux.HandleFunc("POST /api/item/{id}/collection", s.toggleCollection)
	mux.HandleFunc("POST /api/collections", s.createCollection)
	mux.HandleFunc("POST /api/collections/{id}/rename", s.renameCollection)
	mux.HandleFunc("POST /api/collections/{id}/delete", s.deleteCollection)
	mux.HandleFunc("POST /api/tv/check", s.tvCheck)
	mux.HandleFunc("POST /api/tv/artmode", s.tvArtMode)
	mux.HandleFunc("POST /api/tv/sync", s.tvSync)
	return logRequests(mux)
}

func cacheFor(h http.Handler, secs int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(secs))
		h.ServeHTTP(w, r)
	})
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t0 := time.Now()
			h.ServeHTTP(w, r)
			log.Printf("%s %s %.1fs", r.Method, r.URL.Path, time.Since(t0).Seconds())
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---- pages ----

type galleryPage struct {
	Items       []library.Item
	Collections []library.Collection
	Filter      library.Filter
	All, OnTV   int
	Heading     string
	TV          TVState
	LastSync    string
	MaxLoss     float64
	Browsing    bool
}

func (s *Server) gallery(w http.ResponseWriter, r *http.Request) {
	f := library.Filter{}
	var err error
	if f.Collection, err = optionalID(r.URL.Query().Get("c")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.URL.Query().Get("tv") {
	case "on":
		f.OnTV = true
	case "off":
		f.NotOnTV = true
	}
	items, err := s.Lib.List(f)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	colls, all, onTV, err := s.sideData()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	p := galleryPage{Items: items, Collections: colls, Filter: f, All: all, OnTV: onTV, TV: s.tvState(),
		LastSync: s.lastSync(), MaxLoss: s.Lib.MaxLoss, Heading: "All pictures"}
	switch {
	case f.OnTV:
		p.Heading = "On the TV"
	case f.NotOnTV:
		p.Heading = "Not on the TV"
	}
	for _, c := range colls {
		if c.ID == f.Collection {
			p.Heading = c.Name
		}
	}
	s.render(w, "gallery.html", p)
}

type itemPage struct {
	Item        library.Item
	Collections []library.Collection
	TV          TVState
	Prev, Next  int64
}

func (s *Server) itemPage(w http.ResponseWriter, r *http.Request) {
	it, ok := s.item(w, r)
	if !ok {
		return
	}
	colls, err := s.Lib.Collections()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	all, err := s.Lib.List(library.Filter{})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	p := itemPage{Item: it, Collections: colls, TV: s.tvState()}
	for i, x := range all {
		if x.ID == it.ID {
			if i > 0 {
				p.Prev = all[i-1].ID
			}
			if i+1 < len(all) {
				p.Next = all[i+1].ID
			}
		}
	}
	s.render(w, "item.html", p)
}

// sideData is what the library sidebar shows.
func (s *Server) sideData() (colls []library.Collection, all, onTV int, err error) {
	if colls, err = s.Lib.Collections(); err != nil {
		return nil, 0, 0, err
	}
	all, onTV, err = s.Lib.Counts()
	return colls, all, onTV, err
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.tpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var p string
	switch r.PathValue("kind") {
	case "thumb":
		p = s.Lib.ThumbPath(id)
	case "fitted":
		p = s.Lib.FittedPath(id)
	case "original":
		it, err := s.Lib.Get(id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		p = s.Lib.OriginalPath(it)
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, p)
}

// ---- library actions ----

type importResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	ID     int64  `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Import limits. A file or request past one is refused with the reason,
// never read in part.
const (
	maxImportFile    = 200 << 20 // one picture
	maxImportFiles   = 100       // pictures in one import
	maxImportRequest = 2 << 30   // the whole upload
)

func (s *Server) importFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImportRequest)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		reply(w, 400, false, "could not read the upload: "+err.Error(), nil)
		return
	}
	coll, err := optionalID(r.FormValue("collection"))
	if err != nil {
		reply(w, 400, false, err.Error(), nil)
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) > maxImportFiles {
		reply(w, 400, false, fmt.Sprintf("%d pictures in one import; the limit is %d", len(files), maxImportFiles), nil)
		return
	}
	var results []importResult
	for _, fh := range files {
		res := importResult{Name: fh.Filename}
		it, err := s.importOne(fh)
		if err == nil {
			res.OK, res.ID = true, it.ID
			res.Note = fmt.Sprintf("cropped %.0f %%", it.Loss*100)
			if it.Upscaled() {
				res.Note += fmt.Sprintf(", upscaled from %dx%d", it.CropW, it.CropH)
			}
			if coll != 0 {
				if err := s.Lib.SetInCollection(it.ID, coll, true); err != nil {
					res.Note += "; not added to the collection: " + err.Error()
				}
			}
		} else {
			res.Reason = err.Error()
		}
		results = append(results, res)
	}
	ok := 0
	for _, x := range results {
		if x.OK {
			ok++
		}
	}
	reply(w, 200, ok > 0 || len(results) == 0, fmt.Sprintf("%d of %d imported", ok, len(results)), map[string]any{"results": results})
}

func (s *Server) importOne(fh *multipart.FileHeader) (library.Item, error) {
	if fh.Size > maxImportFile {
		return library.Item{}, fmt.Errorf("%d MB is over the %d MB limit for one picture", fh.Size>>20, maxImportFile>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return library.Item{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return library.Item{}, err
	}
	return s.Lib.Import(data, fh.Filename, library.Meta{})
}

// optionalID reads an id from a form or query value; "" means none (0).
func optionalID(v string) (int64, error) {
	if v == "" {
		return 0, nil
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%q is not an id", v)
	}
	return id, nil
}

// requiredID reads an id that must be there.
func requiredID(v string) (int64, error) {
	id, err := optionalID(v)
	if err == nil && id == 0 {
		err = errors.New("an id is missing")
	}
	return id, err
}

func (s *Server) saveMeta(w http.ResponseWriter, r *http.Request) {
	it, ok := s.item(w, r)
	if !ok {
		return
	}
	m := library.Meta{Title: r.FormValue("title"), Artist: r.FormValue("artist"), Year: r.FormValue("year"),
		School: r.FormValue("school"), Medium: r.FormValue("medium"), Source: r.FormValue("source"), URL: r.FormValue("url")}
	if err := s.Lib.Update(it.ID, m); err != nil {
		reply(w, 500, false, err.Error(), nil)
		return
	}
	reply(w, 200, true, "Saved", nil)
}

func (s *Server) deleteItem(w http.ResponseWriter, r *http.Request) {
	it, ok := s.item(w, r)
	if !ok {
		return
	}
	if err := s.Lib.Delete(it.ID); err != nil {
		reply(w, 500, false, err.Error(), nil)
		return
	}
	msg := "Deleted from the library"
	if it.TVID != "" {
		msg += ". The copy on the TV (" + it.TVID + ") stays until you remove it there or with Sync"
	}
	reply(w, 200, true, msg, map[string]any{"redirect": "/"})
}

func (s *Server) toggleCollection(w http.ResponseWriter, r *http.Request) {
	it, ok := s.item(w, r)
	if !ok {
		return
	}
	c, err := requiredID(r.FormValue("collection"))
	if err != nil {
		reply(w, 400, false, err.Error(), nil)
		return
	}
	in := r.FormValue("in") == "1"
	if err := s.Lib.SetInCollection(it.ID, c, in); err != nil {
		reply(w, 500, false, err.Error(), nil)
		return
	}
	reply(w, 200, true, "Saved", nil)
}

func (s *Server) createCollection(w http.ResponseWriter, r *http.Request) {
	id, err := s.Lib.CreateCollection(r.FormValue("name"))
	if err != nil {
		reply(w, 400, false, err.Error(), nil)
		return
	}
	reply(w, 200, true, "Collection created", map[string]any{"redirect": fmt.Sprintf("/?c=%d", id)})
}

func (s *Server) renameCollection(w http.ResponseWriter, r *http.Request) {
	id, err := requiredID(r.PathValue("id"))
	if err != nil {
		reply(w, 400, false, err.Error(), nil)
		return
	}
	if err := s.Lib.RenameCollection(id, r.FormValue("name")); err != nil {
		reply(w, 400, false, err.Error(), nil)
		return
	}
	reply(w, 200, true, "Renamed", map[string]any{"reload": true})
}

func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	id, err := requiredID(r.PathValue("id"))
	if err != nil {
		reply(w, 400, false, err.Error(), nil)
		return
	}
	if err := s.Lib.DeleteCollection(id); err != nil {
		reply(w, 500, false, err.Error(), nil)
		return
	}
	reply(w, 200, true, "Collection deleted; its pictures stay in the library", map[string]any{"redirect": "/"})
}

// ---- TV actions: each one short attempt, started by a person ----

func tvCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Minute)
}

// tvFail turns a TV error into words and remembers that the TV did not answer.
func (s *Server) tvFail(w http.ResponseWriter, err error) {
	msg := err.Error()
	reachable := true
	var te *tv.TVError
	switch {
	case errors.Is(err, tv.ErrUnreachable):
		msg, reachable = "TV not reachable. It is off, asleep or off Wi-Fi; nothing was sent to wake it.", false
	case errors.Is(err, tv.ErrNoAddress):
		msg, reachable = "No TV address is set. Start Frame Gallery with FRAME_TV=<the TV's IP>.", false
	case errors.Is(err, tv.ErrBusy):
		msg = "Another TV action is still running."
	case errors.Is(err, tv.ErrRefused):
		msg = "The TV refused this app. Allow “Frame Gallery” on the TV (Settings › General › External Device Manager › Device Connection Manager), then try again."
	case errors.As(err, &te):
		msg = te.Error()
	}
	if !errors.Is(err, tv.ErrBusy) {
		s.mu.Lock()
		s.last.At, s.last.Reachable, s.last.Message = time.Now(), reachable, msg
		s.mu.Unlock()
	}
	reply(w, 502, false, msg, nil)
}

func (s *Server) tvOK(update func(*TVState)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last.At, s.last.Reachable, s.last.Message = time.Now(), true, ""
	if update != nil {
		update(&s.last)
	}
}

func (s *Server) tvState() TVState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Server) tvCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := tvCtx(r)
	defer cancel()
	st, err := s.TV.Status(ctx)
	if err != nil {
		s.tvFail(w, err)
		return
	}
	s.tvOK(func(t *TVState) { t.ArtMode, t.Current, t.API = st.ArtMode, st.Current, st.APIVersion })
	msg := "TV answered; art mode " + orDash(st.ArtMode)
	if st.Current != "" {
		msg += ", showing " + s.describeTV(st.Current)
	}
	reply(w, 200, true, msg, map[string]any{"art_mode": st.ArtMode, "current": st.Current, "api": st.APIVersion})
}

func (s *Server) describeTV(cid string) string {
	items, err := s.Lib.List(library.Filter{OnTV: true})
	if err != nil {
		log.Printf("describe %s: %v", cid, err)
		return cid
	}
	for _, it := range items {
		if it.TVID == cid {
			return "“" + it.Title + "”"
		}
	}
	return cid
}

func (s *Server) tvArtMode(w http.ResponseWriter, r *http.Request) {
	on := r.FormValue("on") == "1"
	ctx, cancel := tvCtx(r)
	defer cancel()
	if err := s.TV.SetArtMode(ctx, on); err != nil {
		s.tvFail(w, err)
		return
	}
	v := map[bool]string{true: "on", false: "off"}[on]
	s.tvOK(func(t *TVState) { t.ArtMode = v })
	reply(w, 200, true, "Art mode "+v, nil)
}

func (s *Server) show(w http.ResponseWriter, r *http.Request) {
	it, ok := s.item(w, r)
	if !ok {
		return
	}
	ctx, cancel := tvCtx(r)
	defer cancel()
	uploaded := false
	if it.TVID == "" {
		cid, err := s.uploadItem(ctx, it)
		if err != nil {
			s.tvFail(w, err)
			return
		}
		it.TVID, uploaded = cid, true
	}
	if err := s.TV.Show(ctx, it.TVID); err != nil {
		s.tvFail(w, err)
		return
	}
	s.tvOK(func(t *TVState) { t.Current = it.TVID })
	msg := "On the TV now"
	if uploaded {
		msg = "Uploaded and on the TV now"
	}
	reply(w, 200, true, msg, map[string]any{"tv_id": it.TVID})
}

func (s *Server) uploadItem(ctx context.Context, it library.Item) (string, error) {
	data, err := os.ReadFile(s.Lib.FittedPath(it.ID))
	if err != nil {
		return "", err
	}
	cid, err := s.TV.Upload(ctx, data)
	if err != nil {
		return "", err
	}
	if err := s.Lib.SetTV(it.ID, cid); err != nil {
		return cid, err
	}
	return cid, nil
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	it, ok := s.item(w, r)
	if !ok {
		return
	}
	if it.TVID != "" {
		reply(w, 200, true, "Already on the TV as "+it.TVID+" (Sync to re-check)", nil)
		return
	}
	ctx, cancel := tvCtx(r)
	defer cancel()
	cid, err := s.uploadItem(ctx, it)
	if err != nil {
		s.tvFail(w, err)
		return
	}
	s.tvOK(nil)
	reply(w, 200, true, "Uploaded as "+cid, map[string]any{"tv_id": cid})
}

func (s *Server) removeFromTV(w http.ResponseWriter, r *http.Request) {
	it, ok := s.item(w, r)
	if !ok {
		return
	}
	if it.TVID == "" {
		reply(w, 200, true, "Not on the TV", nil)
		return
	}
	ctx, cancel := tvCtx(r)
	defer cancel()
	if err := s.TV.Delete(ctx, it.TVID); err != nil {
		s.tvFail(w, err)
		return
	}
	s.tvOK(nil)
	if err := s.Lib.SetTV(it.ID, ""); err != nil {
		reply(w, 500, false, "Removed from the TV, but the library still marks it as there: "+err.Error(), nil)
		return
	}
	reply(w, 200, true, "Removed from the TV; it stays in the library", nil)
}

func (s *Server) tvSync(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := tvCtx(r)
	defer cancel()
	items, err := s.TV.List(ctx)
	if err != nil {
		s.tvFail(w, err)
		return
	}
	ids := map[string]bool{}
	var uploads []string
	store := 0
	for _, it := range items {
		ids[it.ContentID] = true
		if strings.HasPrefix(it.ContentID, "MY_") {
			uploads = append(uploads, it.ContentID)
		} else {
			store++
		}
	}
	res, err := s.Lib.ApplySync(ids, uploads, store)
	if err != nil {
		reply(w, 500, false, err.Error(), nil)
		return
	}
	s.tvOK(nil)
	msg := fmt.Sprintf("%d library pictures are on the TV", res.Confirmed)
	if res.Gone > 0 {
		msg += fmt.Sprintf(", %d are no longer there", res.Gone)
	}
	if len(res.Unknown) > 0 {
		msg += fmt.Sprintf("; the TV also holds %d uploads this library does not know", len(res.Unknown))
	}
	reply(w, 200, true, msg, map[string]any{"unknown": res.Unknown, "reload": true})
}

// ---- helpers ----

func (s *Server) item(w http.ResponseWriter, r *http.Request) (library.Item, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return library.Item{}, false
	}
	it, err := s.Lib.Get(id)
	if errors.Is(err, library.ErrNotFound) {
		http.NotFound(w, r)
		return it, false
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return it, false
	}
	return it, true
}

func (s *Server) lastSync() string {
	v, err := s.Lib.State("last_sync")
	if err != nil || v == "" {
		if err != nil {
			log.Printf("last sync: %v", err)
		}
		return ""
	}
	t, err := time.Parse(time.RFC3339, strings.Split(v, "|")[0])
	if err != nil {
		log.Printf("last sync %q: %v", v, err)
		return ""
	}
	return "last sync " + ago(t)
}

func reply(w http.ResponseWriter, code int, ok bool, msg string, extra map[string]any) {
	out := map[string]any{"ok": ok, "message": msg}
	for k, v := range extra {
		out[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(out); err != nil {
		log.Printf("reply: %v", err) // the client went away
	}
}

func orDash(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return t.Local().Format("2006-01-02")
}
