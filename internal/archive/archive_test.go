package archive_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	comicarchive "github.com/Bloem-Studios/bloem-community-crowquillx-comic-pages/internal/archive"
)

func protocolLimits() comicarchive.Limits {
	return comicarchive.Limits{
		MaxEntries:         2048,
		MaxPageBytes:       32 << 20,
		MaxTotalBytes:      1 << 30,
		MaxArchiveBytes:    512 << 20,
		MaxDictionaryBytes: 64 << 20,
	}
}

func fixturePath(name string) string {
	_, source, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(source), "../../testdata/fixtures", name)
}

func fixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRARFixturesPreserveBytesAndNaturalSort(t *testing.T) {
	for _, archiveName := range []string{
		"rar40-normal.cbr",
		"rar40-solid.cbr",
		"rar50-normal.cbr",
		"rar50-solid.cbr",
	} {
		t.Run(archiveName, func(t *testing.T) {
			outputDir := t.TempDir()
			pages, err := comicarchive.Extract(context.Background(), fixturePath(archiveName), outputDir, protocolLimits())
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != 3 {
				t.Fatalf("got %d pages, want 3", len(pages))
			}
			wantNames := []string{"page1.png", "page2.png", "page10.png"}
			for i, page := range pages {
				if page.Name != wantNames[i] {
					t.Errorf("page %d name = %q, want %q", i, page.Name, wantNames[i])
				}
				if page.MediaType != "image/png" {
					t.Errorf("page %d media type = %q, want image/png", i, page.MediaType)
				}
				want := fixtureBytes(t, wantNames[i])
				got, readErr := os.ReadFile(page.File)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !bytes.Equal(got, want) {
					t.Errorf("page %d bytes changed", i)
				}
				if page.Size != int64(len(got)) {
					t.Errorf("page %d size = %d, want %d", i, page.Size, len(got))
				}
				if filepath.Dir(page.File) != outputDir {
					t.Errorf("page %d escaped output directory: %q", i, page.File)
				}
				wantFile := filepath.Join(outputDir, fmt.Sprintf("page-%06d", i+1))
				if page.File != wantFile {
					t.Errorf("page %d output = %q, want %q", i, page.File, wantFile)
				}
			}
		})
	}
}

func TestRARDictionaryLimitIsPassedToDecoder(t *testing.T) {
	limits := protocolLimits()
	limits.MaxDictionaryBytes = 1
	_, err := comicarchive.Extract(context.Background(), fixturePath("rar40-normal.cbr"), t.TempDir(), limits)
	if !errors.Is(err, comicarchive.ErrDictionaryTooLarge) {
		t.Fatalf("dictionary limit error = %v", err)
	}
}

type zipEntry struct {
	name  string
	data  []byte
	mode  os.FileMode
	flags uint16
	extra []byte
}

func writeZip(t *testing.T, path string, entries ...zipEntry) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Store, Flags: entry.flags, Extra: entry.extra}
		if entry.mode != 0 {
			header.SetMode(entry.mode)
		}
		writer, createErr := zw.CreateHeader(header)
		if createErr != nil {
			_ = f.Close()
			t.Fatal(createErr)
		}
		if _, writeErr := writer.Write(entry.data); writeErr != nil {
			_ = f.Close()
			t.Fatal(writeErr)
		}
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestZIPTraversalSortingAndNonImages(t *testing.T) {
	root := t.TempDir()
	inputPath := filepath.Join(root, "book.cbz")
	writeZip(t, inputPath,
		zipEntry{name: "page10.png", data: fixtureBytes(t, "page10.png")},
		zipEntry{name: "../../escape.txt", data: []byte("skip me")},
		zipEntry{name: "page2.png", data: fixtureBytes(t, "page2.png")},
		zipEntry{name: "page1.png", data: fixtureBytes(t, "page1.png")},
	)
	outputDir := filepath.Join(root, "out")
	pages, err := comicarchive.Extract(context.Background(), inputPath, outputDir, protocolLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 3 {
		t.Fatalf("got %d pages, want 3", len(pages))
	}
	for i, want := range []string{"page1.png", "page2.png", "page10.png"} {
		if pages[i].Name != want {
			t.Errorf("page %d name = %q, want %q", i, pages[i].Name, want)
		}
		if filepath.Dir(pages[i].File) != outputDir || filepath.Base(pages[i].File) != fmt.Sprintf("page-%06d", i+1) {
			t.Errorf("page %d has non-opaque output %q", i, pages[i].File)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("traversal created a file outside output: %v", err)
	}
}

func TestLimitsAreAppliedToDecodedBytesAndEntries(t *testing.T) {
	page := fixtureBytes(t, "page1.png")
	for _, test := range []struct {
		name string
		lim  comicarchive.Limits
		want error
		data []zipEntry
	}{
		{
			name: "page",
			lim:  limitsWith(func(l *comicarchive.Limits) { l.MaxPageBytes = int64(len(page) - 1) }),
			want: comicarchive.ErrPageTooLarge,
			data: []zipEntry{{name: "page.png", data: page}},
		},
		{
			name: "total",
			lim:  limitsWith(func(l *comicarchive.Limits) { l.MaxTotalBytes = int64(2*len(page) - 1) }),
			want: comicarchive.ErrTotalBytesExceeded,
			data: []zipEntry{{name: "one.png", data: page}, {name: "two.png", data: page}},
		},
		{
			name: "entries",
			lim:  limitsWith(func(l *comicarchive.Limits) { l.MaxEntries = 1 }),
			want: comicarchive.ErrEntryLimitExceeded,
			data: []zipEntry{{name: "one.png", data: page}, {name: "two.png", data: page}},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			inputPath := filepath.Join(root, "book.cbz")
			writeZip(t, inputPath, test.data...)
			_, err := comicarchive.Extract(context.Background(), inputPath, filepath.Join(root, "out"), test.lim)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, test.want)
			}
		})
	}
}

func limitsWith(change func(*comicarchive.Limits)) comicarchive.Limits {
	limits := protocolLimits()
	change(&limits)
	return limits
}

func TestArchiveLimitCancellationAndCorruption(t *testing.T) {
	page := fixtureBytes(t, "page1.png")
	root := t.TempDir()
	inputPath := filepath.Join(root, "book.cbz")
	writeZip(t, inputPath, zipEntry{name: "page.png", data: page})

	archiveLimit := protocolLimits()
	archiveLimit.MaxArchiveBytes = 1
	if _, err := comicarchive.Extract(context.Background(), inputPath, filepath.Join(root, "archive-limit"), archiveLimit); !errors.Is(err, comicarchive.ErrArchiveTooLarge) {
		t.Fatalf("archive limit error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := comicarchive.Extract(ctx, inputPath, filepath.Join(root, "cancelled"), protocolLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}

	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "broken.zip", data: []byte("PK\x03\x04broken")},
		{name: "broken.cbr", data: []byte("Rar!\x1a\x07\x00broken")},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.name)
			if err := os.WriteFile(path, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := comicarchive.Extract(context.Background(), path, filepath.Join(root, "corrupt-"+test.name), protocolLimits()); !errors.Is(err, comicarchive.ErrCorruptArchive) {
				t.Fatalf("corrupt error = %v", err)
			}
		})
	}
}

func TestZIPRejectsSymlinkEncryptionAndInvalidImage(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name  string
		entry zipEntry
		want  error
	}{
		{
			name:  "symlink",
			entry: zipEntry{name: "page.png", data: []byte("../../outside"), mode: os.ModeSymlink | 0777},
			want:  comicarchive.ErrUnsafeEntry,
		},
		{
			name:  "encrypted",
			entry: zipEntry{name: "page.png", data: fixtureBytes(t, "page1.png"), flags: 1},
			want:  comicarchive.ErrEncryptedArchive,
		},
		{
			name:  "invalid image",
			entry: zipEntry{name: "page.png", data: []byte("not a png")},
			want:  comicarchive.ErrInvalidImage,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.name+".cbz")
			writeZip(t, path, test.entry)
			_, err := comicarchive.Extract(context.Background(), path, filepath.Join(root, test.name+"-out"), protocolLimits())
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, test.want)
			}
		})
	}
}
