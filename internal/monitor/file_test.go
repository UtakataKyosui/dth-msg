package monitor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStorePersistence(t *testing.T) {
	f := FileStore{Path: filepath.Join(t.TempDir(), "state.json")}
	if _, err := f.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := State{Confirmed: true, Messages: map[string]string{"discord": "goodbye"}, Delivered: map[string]bool{"discord": true}}
	if err := f.Save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	got, err := f.Load(context.Background())
	if err != nil || !got.Confirmed || !got.Delivered["discord"] || got.Messages["discord"] != "goodbye" {
		t.Fatalf("state not preserved: %+v %v", got, err)
	}
	info, _ := os.Stat(f.Path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("state permissions must be 0600")
	}
	if err := os.WriteFile(f.Path, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Load(context.Background()); err == nil {
		t.Fatal("corrupt state must not reset monitoring")
	}
}
