package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Filip-Pajalic/frame-gallery/pkg/tv"
)

// tvCommand is a one-shot TV call from the command line, for checking the
// protocol by hand: frame tv status|list|show ID|delete ID…|upload FILE.
func tvCommand(c *tv.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: frame tv status|list|show ID|delete ID…|upload FILE.jpg|artmode on|off")
	}
	ctx := context.Background()
	out := func(v any) {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return
		}
		fmt.Println(string(b))
	}
	t0 := time.Now()
	defer func() { fmt.Fprintf(os.Stderr, "%.1f s\n", time.Since(t0).Seconds()) }()
	switch args[0] {
	case "status":
		st, err := c.Status(ctx)
		if err != nil {
			return err
		}
		out(st)
	case "list":
		items, err := c.List(ctx)
		if err != nil {
			return err
		}
		for _, it := range items {
			fmt.Printf("%-16s %-10s %-24s %dx%d\n", it.ContentID, it.CategoryID, it.Matte, it.Width, it.Height)
		}
		fmt.Fprintf(os.Stderr, "%d items\n", len(items))
	case "show":
		return c.Show(ctx, args[1])
	case "delete":
		return c.Delete(ctx, args[1:]...)
	case "artmode":
		return c.SetArtMode(ctx, len(args) > 1 && args[1] == "on")
	case "upload":
		b, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		cid, err := c.Upload(ctx, b)
		if err != nil {
			return err
		}
		fmt.Println(cid)
	default:
		return fmt.Errorf("unknown tv command %q", args[0])
	}
	return nil
}
