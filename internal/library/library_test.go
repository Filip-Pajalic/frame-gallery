package library

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"testing"
)

func pic(t *testing.T, w, h int) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.RGBA{uint8(x), uint8(y), 90, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestLoss(t *testing.T) {
	for _, c := range []struct {
		w, h int
		want float64
	}{{3840, 2160, 0}, {1600, 1200, 0.25}, {1000, 1000, 0.4375}, {3200, 900, 0.5}} {
		if got := Loss(c.w, c.h); math.Abs(got-c.want) > 1e-3 {
			t.Errorf("Loss(%d,%d) = %.4f, want %.4f", c.w, c.h, got, c.want)
		}
	}
}

func TestFitRule(t *testing.T) {
	f, err := Fit(pic(t, 800, 600), 0.33) // 4:3 loses 25 %
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(f.JPEG))
	if err != nil || cfg.Width != FitW || cfg.Height != FitH {
		t.Fatalf("fitted %dx%d %v", cfg.Width, cfg.Height, err)
	}
	if f.CropW != 800 || f.CropH != 450 {
		t.Fatalf("crop %dx%d", f.CropW, f.CropH)
	}
	_, err = Fit(pic(t, 600, 600), 0.33) // square loses 44 %
	var r *Refused
	if !errors.As(err, &r) {
		t.Fatalf("square: want Refused, got %v", err)
	}
	if _, err := Fit([]byte("not an image"), 0.33); !errors.As(err, &r) {
		t.Fatalf("garbage: want Refused, got %v", err)
	}
}

func TestOrient(t *testing.T) {
	im := image.NewRGBA(image.Rect(0, 0, 4, 2))
	im.Set(0, 0, color.RGBA{255, 0, 0, 255})
	r := orient(im, 6) // rotate 90° clockwise
	if b := r.Bounds(); b.Dx() != 2 || b.Dy() != 4 {
		t.Fatalf("bounds %v", b)
	}
	if c := color.RGBAModel.Convert(r.At(1, 0)).(color.RGBA); c.R != 255 {
		t.Fatalf("top-left pixel should be at top-right, got %v", c)
	}
}

func TestStoreAndSync(t *testing.T) {
	l, err := Open(t.TempDir(), 0.33)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	a, err := l.Import(pic(t, 1600, 900), "a.jpg", Meta{Title: "A", TVID: "MY_F0001"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := l.Import(pic(t, 1600, 1000), "b.jpg", Meta{TVID: "MY_F0002"})
	if err != nil {
		t.Fatal(err)
	}
	if b.Title != "b" {
		t.Fatalf("title from file name: %q", b.Title)
	}
	if _, err := l.Import(pic(t, 500, 500), "sq.jpg", Meta{}); err == nil {
		t.Fatal("square imported")
	}
	if all, _ := l.List(Filter{}); len(all) != 2 {
		t.Fatalf("refused import left a row: %d", len(all))
	}
	for _, p := range []string{l.OriginalPath(a), l.FittedPath(a.ID), l.ThumbPath(a.ID)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	c, _ := l.CreateCollection("Dutch")
	if err := l.SetInCollection(a.ID, c, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.List(Filter{Collection: c}); len(got) != 1 || got[0].ID != a.ID {
		t.Fatalf("collection filter %+v", got)
	}
	res, err := l.ApplySync(map[string]bool{"MY_F0001": true, "MY_F0099": true}, []string{"MY_F0001", "MY_F0099"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if res.Confirmed != 1 || res.Gone != 1 || len(res.Unknown) != 1 || res.Unknown[0] != "MY_F0099" {
		t.Fatalf("sync %+v", res)
	}
	if got, _ := l.Get(b.ID); got.TVID != "" {
		t.Fatalf("b should be marked off the TV: %q", got.TVID)
	}
	if err := l.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(l.FittedPath(a.ID)); !os.IsNotExist(err) {
		t.Fatal("fitted file left behind")
	}
	if cs, _ := l.Collections(); cs[0].Count != 0 {
		t.Fatalf("collection still counts the deleted item: %+v", cs)
	}
}

func TestOpenRefusesBadLimit(t *testing.T) {
	for _, v := range []float64{0, 1, 1.5, -0.1} {
		if _, err := Open(t.TempDir(), v); err == nil {
			t.Errorf("Open accepted crop limit %v", v)
		}
	}
}

// Deleting a picture whose files are already gone is not an error.
func TestDeleteWithMissingFiles(t *testing.T) {
	l, err := Open(t.TempDir(), 0.33)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	it, err := l.Import(pic(t, 1600, 900), "a.jpg", Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(l.ThumbPath(it.ID)); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(it.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
}
