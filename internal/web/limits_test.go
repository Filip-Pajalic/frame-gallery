package web

import (
	"mime/multipart"
	"testing"
)

func TestIDs(t *testing.T) {
	for v, ok := range map[string]bool{"": true, "7": true, "0": false, "-3": false, "x": false} {
		if _, err := optionalID(v); (err == nil) != ok {
			t.Errorf("optionalID(%q): err %v", v, err)
		}
	}
	if _, err := requiredID(""); err == nil {
		t.Error("requiredID accepted a missing id")
	}
}

// A file over the limit is refused before it is opened, not read in part.
func TestImportRefusesOversize(t *testing.T) {
	s := &Server{}
	_, err := s.importOne(&multipart.FileHeader{Filename: "huge.jpg", Size: maxImportFile + 1})
	if err == nil {
		t.Fatal("oversize file accepted")
	}
}
