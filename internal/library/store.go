// Package library is the source of truth: originals, fitted 3840x2160 JPEGs
// and thumbnails on disk, metadata and collections in one SQLite file.
package library

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Item struct {
	ID       int64
	Title    string
	Artist   string
	Year     string
	School   string
	Medium   string
	Source   string // credit line
	URL      string // where the original came from
	Ext      string // extension of the stored original
	Width    int    // original
	Height   int
	CropW    int // region kept, before scaling
	CropH    int
	Loss     float64
	Added    time.Time
	TVID     string    // content id on the TV, "" when not there (as far as we know)
	TVSeen   time.Time // when TVID was last confirmed (upload or sync)
	Collects []int64
}

// Upscaled tells whether the kept region was smaller than the screen.
func (it Item) Upscaled() bool { return it.CropW < FitW }

type Collection struct {
	ID    int64
	Name  string
	Count int
}

type Library struct {
	Dir     string
	MaxLoss float64
	db      *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS items (
  id      INTEGER PRIMARY KEY,
  title   TEXT NOT NULL DEFAULT '',
  artist  TEXT NOT NULL DEFAULT '',
  year    TEXT NOT NULL DEFAULT '',
  school  TEXT NOT NULL DEFAULT '',
  medium  TEXT NOT NULL DEFAULT '',
  source  TEXT NOT NULL DEFAULT '',
  url     TEXT NOT NULL DEFAULT '',
  ext     TEXT NOT NULL,
  width   INTEGER NOT NULL, height INTEGER NOT NULL,
  crop_w  INTEGER NOT NULL, crop_h INTEGER NOT NULL,
  loss    REAL NOT NULL,
  added   TEXT NOT NULL,
  tv_id   TEXT NOT NULL DEFAULT '',
  tv_seen TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS collections (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);
CREATE TABLE IF NOT EXISTS collection_items (
  collection_id INTEGER NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
  item_id       INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  PRIMARY KEY (collection_id, item_id)
);
CREATE TABLE IF NOT EXISTS state (k TEXT PRIMARY KEY, v TEXT NOT NULL);
`

func Open(dir string, maxLoss float64) (*Library, error) {
	if !(maxLoss > 0 && maxLoss < 1) {
		return nil, fmt.Errorf("the crop limit must be between 0 and 1, not %v", maxLoss)
	}
	for _, d := range []string{"originals", "fitted", "thumbs"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "library.db")+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		return nil, err
	}
	return &Library{Dir: dir, MaxLoss: maxLoss, db: db}, nil
}

func (l *Library) Close() error { return l.db.Close() }

func (l *Library) OriginalPath(it Item) string {
	return filepath.Join(l.Dir, "originals", fmt.Sprintf("%d%s", it.ID, it.Ext))
}
func (l *Library) FittedPath(id int64) string {
	return filepath.Join(l.Dir, "fitted", fmt.Sprintf("%d.jpg", id))
}
func (l *Library) ThumbPath(id int64) string {
	return filepath.Join(l.Dir, "thumbs", fmt.Sprintf("%d.jpg", id))
}

// Meta is what an import knows about a picture besides its bytes.
type Meta struct {
	Title, Artist, Year, School, Medium, Source, URL string
	TVID                                             string // already on the TV under this id
}

// Import applies the full-screen rule and stores the original, the fitted
// JPEG and the thumbnail. A refused picture leaves nothing behind.
func (l *Library) Import(data []byte, filename string, m Meta) (Item, error) {
	f, err := Fit(data, l.MaxLoss)
	if err != nil {
		return Item{}, err
	}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == "" || len(ext) > 6 {
		ext = ".img"
	}
	if m.Title == "" {
		m.Title = strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename))
	}
	now := time.Now().UTC()
	it := Item{Title: m.Title, Artist: m.Artist, Year: m.Year, School: m.School, Medium: m.Medium,
		Source: m.Source, URL: m.URL, Ext: ext, Width: f.Width, Height: f.Height, CropW: f.CropW,
		CropH: f.CropH, Loss: f.Loss, Added: now, TVID: m.TVID}
	seen := ""
	if m.TVID != "" {
		seen = now.Format(time.RFC3339)
	}
	tx, err := l.db.Begin()
	if err != nil {
		return Item{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO items(title,artist,year,school,medium,source,url,ext,width,height,crop_w,crop_h,loss,added,tv_id,tv_seen)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, it.Title, it.Artist, it.Year, it.School, it.Medium, it.Source, it.URL,
		it.Ext, it.Width, it.Height, it.CropW, it.CropH, it.Loss, now.Format(time.RFC3339), it.TVID, seen)
	if err != nil {
		return Item{}, err
	}
	if it.ID, err = res.LastInsertId(); err != nil {
		return Item{}, err
	}
	files := map[string][]byte{l.OriginalPath(it): data, l.FittedPath(it.ID): f.JPEG, l.ThumbPath(it.ID): f.Thumb}
	for p, b := range files {
		if err := writeFile(p, b); err != nil {
			paths := make([]string, 0, len(files))
			for q := range files {
				paths = append(paths, q)
			}
			return Item{}, errors.Join(err, removeFiles(paths...))
		}
	}
	return it, tx.Commit()
}

// removeFiles deletes files; one that is already gone is not an error.
func removeFiles(paths ...string) error {
	var errs []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func writeFile(p string, b []byte) error {
	tmp := p + ".part"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

const itemCols = `id,title,artist,year,school,medium,source,url,ext,width,height,crop_w,crop_h,loss,added,tv_id,tv_seen`

func scanItem(sc interface{ Scan(...any) error }) (Item, error) {
	var it Item
	var added, seen string
	err := sc.Scan(&it.ID, &it.Title, &it.Artist, &it.Year, &it.School, &it.Medium, &it.Source, &it.URL, &it.Ext,
		&it.Width, &it.Height, &it.CropW, &it.CropH, &it.Loss, &added, &it.TVID, &seen)
	if err != nil {
		return it, err
	}
	if it.Added, err = time.Parse(time.RFC3339, added); err != nil {
		return it, fmt.Errorf("item %d: bad added time: %w", it.ID, err)
	}
	if seen != "" { // empty means never confirmed on the TV
		if it.TVSeen, err = time.Parse(time.RFC3339, seen); err != nil {
			return it, fmt.Errorf("item %d: bad tv_seen time: %w", it.ID, err)
		}
	}
	return it, nil
}

var ErrNotFound = errors.New("no such picture")

func (l *Library) Get(id int64) (Item, error) {
	it, err := scanItem(l.db.QueryRow(`SELECT `+itemCols+` FROM items WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return it, ErrNotFound
	}
	if err != nil {
		return it, err
	}
	rows, err := l.db.Query(`SELECT collection_id FROM collection_items WHERE item_id=?`, id)
	if err != nil {
		return it, err
	}
	defer rows.Close()
	for rows.Next() {
		var c int64
		if err := rows.Scan(&c); err != nil {
			return it, err
		}
		it.Collects = append(it.Collects, c)
	}
	return it, rows.Err()
}

// Filter picks what the gallery lists.
type Filter struct {
	Collection int64 // 0: all
	OnTV       bool
	NotOnTV    bool
}

func (l *Library) List(f Filter) ([]Item, error) {
	q := `SELECT ` + itemCols + ` FROM items`
	var where []string
	var args []any
	if f.Collection != 0 {
		where = append(where, `id IN (SELECT item_id FROM collection_items WHERE collection_id=?)`)
		args = append(args, f.Collection)
	}
	if f.OnTV {
		where = append(where, `tv_id <> ''`)
	}
	if f.NotOnTV {
		where = append(where, `tv_id = ''`)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	rows, err := l.db.Query(q+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Item
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Counts for the filter list.
func (l *Library) Counts() (all, onTV int, err error) {
	err = l.db.QueryRow(`SELECT count(*), coalesce(sum(tv_id <> ''),0) FROM items`).Scan(&all, &onTV)
	return
}

func (l *Library) Update(id int64, m Meta) error {
	_, err := l.db.Exec(`UPDATE items SET title=?,artist=?,year=?,school=?,medium=?,source=?,url=? WHERE id=?`,
		m.Title, m.Artist, m.Year, m.School, m.Medium, m.Source, m.URL, id)
	return err
}

// SetTV records the TV's content id ("" when it is no longer there).
func (l *Library) SetTV(id int64, tvID string) error {
	seen := ""
	if tvID != "" {
		seen = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := l.db.Exec(`UPDATE items SET tv_id=?, tv_seen=? WHERE id=?`, tvID, seen, id)
	return err
}

// Delete removes a picture and its files from the library. It does not touch the TV.
func (l *Library) Delete(id int64) error {
	it, err := l.Get(id)
	if err != nil {
		return err
	}
	if _, err := l.db.Exec(`DELETE FROM items WHERE id=?`, id); err != nil {
		return err
	}
	return removeFiles(l.OriginalPath(it), l.FittedPath(id), l.ThumbPath(id))
}

// SyncResult is what a sync found.
type SyncResult struct {
	Confirmed int      // library items still on the TV
	Gone      int      // library items the TV no longer has (marked not on TV)
	Unknown   []string // uploads on the TV that the library does not know
	StoreArt  int      // Art Store items on the TV
	At        time.Time
}

// ApplySync marks which library items are on the TV, from the TV's list.
func (l *Library) ApplySync(tvIDs map[string]bool, uploads []string, store int) (SyncResult, error) {
	r := SyncResult{StoreArt: store, At: time.Now().UTC()}
	items, err := l.List(Filter{OnTV: true})
	if err != nil {
		return r, err
	}
	known := map[string]bool{}
	for _, it := range items {
		known[it.TVID] = true
		tvID := ""
		if tvIDs[it.TVID] {
			r.Confirmed++
			tvID = it.TVID
		} else {
			r.Gone++
		}
		if err := l.SetTV(it.ID, tvID); err != nil {
			return r, err
		}
	}
	for _, u := range uploads {
		if !known[u] {
			r.Unknown = append(r.Unknown, u)
		}
	}
	err = l.SetState("last_sync", fmt.Sprintf("%s|%d|%d|%d|%d", r.At.Format(time.RFC3339), r.Confirmed, r.Gone, len(r.Unknown), r.StoreArt))
	return r, err
}

func (l *Library) SetState(k, v string) error {
	_, err := l.db.Exec(`INSERT INTO state(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// State reads a stored value; a key never set reads as "".
func (l *Library) State(k string) (string, error) {
	var v string
	err := l.db.QueryRow(`SELECT v FROM state WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// ---- collections ----

func (l *Library) Collections() ([]Collection, error) {
	rows, err := l.db.Query(`SELECT c.id, c.name, count(ci.item_id) FROM collections c
		LEFT JOIN collection_items ci ON ci.collection_id=c.id GROUP BY c.id ORDER BY c.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Collection
	for rows.Next() {
		var c Collection
		if err := rows.Scan(&c.ID, &c.Name, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (l *Library) CreateCollection(name string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, errors.New("a collection needs a name")
	}
	if _, err := l.db.Exec(`INSERT INTO collections(name) VALUES(?) ON CONFLICT(name) DO NOTHING`, name); err != nil {
		return 0, err
	}
	var id int64
	err := l.db.QueryRow(`SELECT id FROM collections WHERE name=?`, name).Scan(&id)
	return id, err
}

func (l *Library) RenameCollection(id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a collection needs a name")
	}
	_, err := l.db.Exec(`UPDATE collections SET name=? WHERE id=?`, name, id)
	return err
}

func (l *Library) DeleteCollection(id int64) error {
	_, err := l.db.Exec(`DELETE FROM collections WHERE id=?`, id)
	return err
}

func (l *Library) SetInCollection(item, coll int64, in bool) error {
	var err error
	if in {
		_, err = l.db.Exec(`INSERT OR IGNORE INTO collection_items(collection_id,item_id) VALUES(?,?)`, coll, item)
	} else {
		_, err = l.db.Exec(`DELETE FROM collection_items WHERE collection_id=? AND item_id=?`, coll, item)
	}
	return err
}
