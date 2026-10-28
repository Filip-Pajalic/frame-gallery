// Frame Gallery — a local library of pictures for a Samsung The Frame, shown
// on the TV only when someone asks.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Filip-Pajalic/frame-gallery/pkg/tv"
)

func main() {
	fs := flag.NewFlagSet("frame", flag.ExitOnError)
	data := fs.String("data", envOr("FRAME_DATA", "data"), "library directory (SQLite file, originals, fitted JPEGs, thumbnails)")
	addr := fs.String("http", envOr("FRAME_HTTP", ":18000"), "listen address")
	host := fs.String("tv", envOr("FRAME_TV", ""), "TV address (IP or name); required for TV actions")
	port := fs.Int("tv-port", 8001, "TV websocket port (8001 ws, 8002 wss)")
	maxLoss := fs.Float64("max-loss", envFloat("FRAME_MAX_LOSS", 0.33), "largest share of a picture the 16:9 crop may remove")
	_ = fs.Parse(os.Args[1:]) // ExitOnError: Parse exits on a bad flag and never returns an error

	client := &tv.Client{
		Host: *host, Port: *port, Name: "Frame Gallery",
		TokenFile: filepath.Join(*data, "tv-token"),
		Dial:      4 * time.Second, Reply: 15 * time.Second,
	}
	if err := os.MkdirAll(*data, 0o755); err != nil {
		fail(err)
	}

	args := fs.Args()
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "tv":
		err = tvCommand(client, args)
	case "serve":
		err = serve(*addr, *data, *maxLoss, client)
	case "health":
		err = healthCommand(*addr)
	case "import":
		err = importCommand(*data, *maxLoss, args)
	default:
		err = fmt.Errorf("unknown command %q (serve, import, tv, health)", cmd)
	}
	if err != nil {
		fail(err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envFloat(k string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return def
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "frame:", err)
	os.Exit(1)
}
