package silo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPageCacheRejectsUnownedNonemptyRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "foreign-data"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newPageCache(root, 1024, time.Minute); err == nil {
		t.Fatal("unowned nonempty cache root was accepted")
	}

	ownedRoot := t.TempDir()
	cache, err := newPageCache(ownedRoot, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cache.close)
	marker, err := os.ReadFile(filepath.Join(ownedRoot, cacheOwnerMarker))
	if err != nil {
		t.Fatal(err)
	}
	if string(marker) != cacheOwnerContents {
		t.Fatalf("ownership marker = %q", marker)
	}
}

func TestPageCacheExclusiveLeaseProtectsExistingEntries(t *testing.T) {
	root := t.TempDir()
	first, err := newPageCache(root, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(first.close)
	entry := filepath.Join(root, ".entry-existing")
	if err := os.Mkdir(entry, 0o700); err != nil {
		t.Fatal(err)
	}
	if second, err := newPageCache(root, 1024, time.Minute); err == nil {
		second.close()
		t.Fatal("second instance acquired an active cache root")
	}
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("second instance removed the first instance's entry: %v", err)
	}
	first.close()
	reopened, err := newPageCache(root, 1024, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reopened.close)
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatalf("restart did not clean the stale entry: %v", err)
	}
}
