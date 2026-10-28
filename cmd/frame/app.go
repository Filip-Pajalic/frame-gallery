package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Filip-Pajalic/frame-gallery/internal/library"
	"github.com/Filip-Pajalic/frame-gallery/internal/web"
	"github.com/Filip-Pajalic/frame-gallery/pkg/tv"
)

func serve(addr, data string, maxLoss float64, c *tv.Client) error {
	lib, err := library.Open(data, maxLoss)
	if err != nil {
		return err
	}
	defer lib.Close()
	s, err := web.New(lib, c)
	if err != nil {
		return err
	}
	log.Printf("Frame Gallery on %s, library %s, TV %s:%d (contacted only on request)", addr, data, c.Host, c.Port)
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	return srv.ListenAndServe()
}

// healthCommand is the container's health check: the scratch image has no curl.
func healthCommand(addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	c := &http.Client{Timeout: 3 * time.Second}
	r, err := c.Get("http://127.0.0.1:" + port + "/health")
	if err != nil {
		return err
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		return fmt.Errorf("health: HTTP %d", r.StatusCode)
	}
	return nil
}

// seedEntry is one line of a seed manifest: a picture plus what is known about it.
type seedEntry struct {
	File        string `json:"file"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Year        string `json:"year"`
	School      string `json:"school"`
	Medium      string `json:"medium"`
	Source      string `json:"source"`
	URL         string `json:"url"`
	TVContentID string `json:"tv_content_id"`
}

// importCommand imports pictures from the command line, either plain files or
// a JSON manifest (a list of seedEntry, files relative to the manifest).
//
//	frame import a.jpg b.png
//	frame import -collection "Paintings" seed.json
func importCommand(data string, maxLoss float64, args []string) error {
	coll := ""
	if len(args) > 1 && args[0] == "-collection" {
		coll, args = args[1], args[2:]
	}
	if len(args) == 0 {
		return errors.New("usage: frame import [-collection NAME] FILE… | MANIFEST.json")
	}
	lib, err := library.Open(data, maxLoss)
	if err != nil {
		return err
	}
	defer lib.Close()
	var collID int64
	if coll != "" {
		if collID, err = lib.CreateCollection(coll); err != nil {
			return err
		}
	}
	var entries []seedEntry
	if len(args) == 1 && strings.HasSuffix(strings.ToLower(args[0]), ".json") {
		b, err := os.ReadFile(args[0])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(b, &entries); err != nil {
			return fmt.Errorf("%s: %w", args[0], err)
		}
		dir := filepath.Dir(args[0])
		for i := range entries {
			if !filepath.IsAbs(entries[i].File) {
				entries[i].File = filepath.Join(dir, entries[i].File)
			}
		}
	} else {
		for _, a := range args {
			entries = append(entries, seedEntry{File: a})
		}
	}
	ok := 0
	for _, e := range entries {
		t0 := time.Now()
		b, err := os.ReadFile(e.File)
		if err != nil {
			fmt.Printf("SKIP %s: %v\n", e.File, err)
			continue
		}
		it, err := lib.Import(b, filepath.Base(e.File), library.Meta{Title: e.Title, Artist: e.Artist, Year: e.Year,
			School: e.School, Medium: e.Medium, Source: e.Source, URL: e.URL, TVID: e.TVContentID})
		if err != nil {
			fmt.Printf("REFUSED %s: %v\n", filepath.Base(e.File), err)
			continue
		}
		if collID != 0 {
			if err := lib.SetInCollection(it.ID, collID, true); err != nil {
				return fmt.Errorf("%s imported as %d but not added to %q: %w", filepath.Base(e.File), it.ID, coll, err)
			}
		}
		ok++
		fmt.Printf("ok %3d %-10s %3.0f%% %5.1fs  %s\n", it.ID, it.TVID, it.Loss*100, time.Since(t0).Seconds(), it.Title)
	}
	fmt.Printf("%d of %d imported\n", ok, len(entries))
	return nil
}
