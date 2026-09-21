package archive_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"

	comicarchive "github.com/Bloem-Studios/bloem-community-crowquillx-comic-pages/internal/archive"
)

func TestImagePixelLimitBeforeFullDecode(t *testing.T) {
	// These only contain headers. ErrImageTooLarge, rather than a missing
	// pixel-data error, proves rejection happens before full image decoding.
	pngHeader := append([]byte(nil), fixtureBytes(t, "page1.png")[:33]...)
	binary.BigEndian.PutUint32(pngHeader[16:], 100000)
	binary.BigEndian.PutUint32(pngHeader[20:], 100000)
	binary.BigEndian.PutUint32(pngHeader[29:], crc32.ChecksumIEEE(pngHeader[12:29]))
	tests := []struct {
		name string
		data []byte
	}{
		{"huge.png", pngHeader},
		{"huge-canvas.webp", makeWebP("VP8X", webPCanvas(16384, 8192, 0x10))},
		{"huge-lossless.webp", makeWebP("VP8L", webPLosslessHeader(16384, 8192))},
		{"huge-lossy.webp", makeWebP("VP8 ", webPLossyHeader(16383, 8192))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertZIPImageError(t, test.name, test.data, comicarchive.ErrImageTooLarge)
		})
	}
}

func TestWebPPreservesOriginalBytes(t *testing.T) {
	for _, name := range []string{"pattern-lossless.webp", "pattern-lossy.webp", "pattern-alpha.webp"} {
		t.Run(name, func(t *testing.T) {
			data := fixtureBytes(t, name)
			root := t.TempDir()
			input, output := filepath.Join(root, "book.cbz"), filepath.Join(root, "out")
			writeZip(t, input, zipEntry{name: name, data: data})
			pages, err := comicarchive.Extract(context.Background(), input, output, protocolLimits())
			if err != nil {
				t.Fatal(err)
			}
			if len(pages) != 1 || pages[0].MediaType != "image/webp" || pages[0].Size != int64(len(data)) {
				t.Fatalf("unexpected WebP pages: %+v", pages)
			}
			extracted, err := os.ReadFile(pages[0].File)
			if err != nil || !bytes.Equal(extracted, data) {
				t.Fatalf("original WebP bytes changed: %v", err)
			}
		})
	}
}

func TestWebPRejectsMalformedAndAnimatedImages(t *testing.T) {
	// Use a complete, small VP8L bitstream so omitting the dimension check
	// would accept the image, and the regression stays safe with older decoders.
	mismatch := makeWebP("VP8X", webPCanvas(1, 1, 0))
	mismatch = append(mismatch, fixtureBytes(t, "pattern-lossless.webp")[12:]...)
	binary.LittleEndian.PutUint32(mismatch[4:], uint32(len(mismatch)-8))
	assertZIPImageError(t, "mismatched.webp", mismatch, comicarchive.ErrInvalidImage)

	animated := append([]byte(nil), fixtureBytes(t, "pattern-alpha.webp")...)
	animated[20] |= 2
	assertZIPImageError(t, "animated.webp", animated, comicarchive.ErrUnsupportedImageType)
	truncated := fixtureBytes(t, "pattern-lossless.webp")[:21]
	assertZIPImageError(t, "truncated.webp", truncated, comicarchive.ErrInvalidImage)
	malformed := append([]byte(nil), fixtureBytes(t, "pattern-lossless.webp")...)
	binary.LittleEndian.PutUint32(malformed[16:], 0xffffffff)
	assertZIPImageError(t, "bad-chunk-length.webp", malformed, comicarchive.ErrInvalidImage)
}

func assertZIPImageError(t *testing.T, name string, data []byte, want error) {
	t.Helper()
	root := t.TempDir()
	input, output := filepath.Join(root, "book.cbz"), filepath.Join(root, "out")
	writeZip(t, input, zipEntry{name: name, data: data})
	pages, err := comicarchive.Extract(context.Background(), input, output, protocolLimits())
	if !errors.Is(err, want) || pages != nil {
		t.Fatalf("pages = %+v, error = %v, want %v", pages, err, want)
	}
	entries, err := os.ReadDir(output)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed extraction left files: %v, %v", entries, err)
	}
}

func makeWebP(kind string, payload []byte) []byte {
	data := append([]byte("RIFF\x00\x00\x00\x00WEBP"), webPChunk(kind, payload)...)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	return data
}

func webPChunk(kind string, payload []byte) []byte {
	data := make([]byte, 8+len(payload)+len(payload)%2)
	copy(data, kind)
	binary.LittleEndian.PutUint32(data[4:], uint32(len(payload)))
	copy(data[8:], payload)
	return data
}

func webPCanvas(width, height uint32, flags byte) []byte {
	data := make([]byte, 10)
	data[0] = flags
	for i := 0; i < 3; i++ {
		data[4+i] = byte((width - 1) >> (8 * i))
		data[7+i] = byte((height - 1) >> (8 * i))
	}
	return data
}

func webPLosslessHeader(width, height uint32) []byte {
	data := []byte{0x2f, 0, 0, 0, 0}
	binary.LittleEndian.PutUint32(data[1:], (width-1)|(height-1)<<14)
	return data
}

func webPLossyHeader(width, height uint16) []byte {
	data := []byte{0x10, 0, 0, 0x9d, 0x01, 0x2a, 0, 0, 0, 0}
	binary.LittleEndian.PutUint16(data[6:], width)
	binary.LittleEndian.PutUint16(data[8:], height)
	return data
}

func TestZIPDirectoryPreflightRejectsMalformedArchives(t *testing.T) {
	root := t.TempDir()
	input := filepath.Join(root, "original.cbz")
	writeZip(t, input,
		zipEntry{name: "page1.png", data: fixtureBytes(t, "page1.png")},
		zipEntry{name: "page2.png", data: fixtureBytes(t, "page2.png")},
		zipEntry{name: "page10.png", data: fixtureBytes(t, "page10.png")},
	)
	original, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func([]byte, []byte) []byte
		want error
	}{
		{"declared count exceeds cap", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint16(eocd[8:], 3000)
			binary.LittleEndian.PutUint16(eocd[10:], 3000)
			return data
		}, comicarchive.ErrEntryLimitExceeded},
		{"lying count", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint16(eocd[8:], 1)
			binary.LittleEndian.PutUint16(eocd[10:], 1)
			return data
		}, comicarchive.ErrEntryLimitExceeded},
		{"inconsistent count below cap", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint16(eocd[8:], 1)
			binary.LittleEndian.PutUint16(eocd[10:], 1)
			return data
		}, comicarchive.ErrCorruptArchive},
		{"ZIP64 count", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint16(eocd[8:], 0xffff)
			binary.LittleEndian.PutUint16(eocd[10:], 0xffff)
			return data
		}, comicarchive.ErrUnsupportedArchive},
		{"ZIP64 size", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint32(eocd[12:], 0xffffffff)
			return data
		}, comicarchive.ErrUnsupportedArchive},
		{"ZIP64 locator with classic fields", func(data, eocd []byte) []byte {
			locator := make([]byte, 20)
			copy(locator, "PK\x06\x07")
			end := append([]byte(nil), eocd...)
			return append(append(data[:len(data)-22], locator...), end...)
		}, comicarchive.ErrUnsupportedArchive},
		{"multidisk", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint16(eocd[4:], 1)
			return data
		}, comicarchive.ErrUnsupportedArchive},
		{"offset beyond archive", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint32(eocd[16:], uint32(len(data)+1))
			return data
		}, comicarchive.ErrCorruptArchive},
		{"offset correction unsupported", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint32(eocd[16:], binary.LittleEndian.Uint32(eocd[16:])-1)
			return data
		}, comicarchive.ErrCorruptArchive},
		{"directory size exceeds file", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint32(eocd[12:], 0xfffffffe)
			return data
		}, comicarchive.ErrCorruptArchive},
		{"header name exceeds directory", func(data, eocd []byte) []byte {
			offset := binary.LittleEndian.Uint32(eocd[16:])
			binary.LittleEndian.PutUint16(data[offset+28:], 65535)
			return data
		}, comicarchive.ErrCorruptArchive},
		{"header extra exceeds directory", func(data, eocd []byte) []byte {
			offset := binary.LittleEndian.Uint32(eocd[16:])
			binary.LittleEndian.PutUint16(data[offset+30:], 65535)
			return data
		}, comicarchive.ErrCorruptArchive},
		{"header comment exceeds directory", func(data, eocd []byte) []byte {
			offset := binary.LittleEndian.Uint32(eocd[16:])
			binary.LittleEndian.PutUint16(data[offset+32:], 65535)
			return data
		}, comicarchive.ErrCorruptArchive},
		{"directory signature", func(data, eocd []byte) []byte {
			offset := binary.LittleEndian.Uint32(eocd[16:])
			data[offset] = 'X'
			return data
		}, comicarchive.ErrCorruptArchive},
		{"short directory header", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint32(eocd[12:], 45)
			binary.LittleEndian.PutUint32(eocd[16:], uint32(len(data)-22-45))
			return data
		}, comicarchive.ErrCorruptArchive},
		{"truncated comment", func(data, eocd []byte) []byte {
			binary.LittleEndian.PutUint16(eocd[20:], 100)
			return data
		}, comicarchive.ErrCorruptArchive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data := append([]byte(nil), original...)
			eocd := data[len(data)-22:]
			limits := protocolLimits()
			if test.name == "lying count" {
				limits.MaxEntries = 2
			}
			data = test.edit(data, eocd)
			path := filepath.Join(t.TempDir(), "bad.cbz")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			output := t.TempDir()
			pages, err := comicarchive.Extract(context.Background(), path, output, limits)
			if !errors.Is(err, test.want) || pages != nil {
				t.Fatalf("pages = %+v, error = %v, want %v", pages, err, test.want)
			}
			entries, err := os.ReadDir(output)
			if err != nil || len(entries) != 0 {
				t.Fatalf("preflight wrote files: %v, %v", entries, err)
			}
		})
	}
}

func TestClassicZIPAllowsSmallZIP64Entries(t *testing.T) {
	data := fixtureBytes(t, "page1.png")
	extra := make([]byte, 20)
	binary.LittleEndian.PutUint16(extra, 0x0001)
	binary.LittleEndian.PutUint16(extra[2:], 16)
	binary.LittleEndian.PutUint64(extra[4:], uint64(len(data)))
	binary.LittleEndian.PutUint64(extra[12:], uint64(len(data)))
	input := filepath.Join(t.TempDir(), "zip64-entry.cbz")
	writeZip(t, input, zipEntry{name: "page.png", data: data, extra: extra})
	encoded, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	eocd := encoded[len(encoded)-22:]
	central := binary.LittleEndian.Uint32(eocd[16:])
	binary.LittleEndian.PutUint32(encoded[central+20:], 0xffffffff)
	binary.LittleEndian.PutUint32(encoded[central+24:], 0xffffffff)
	if err := os.WriteFile(input, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	pages, err := comicarchive.Extract(context.Background(), input, t.TempDir(), protocolLimits())
	if err != nil || len(pages) != 1 {
		t.Fatalf("small ZIP64 entry: pages = %+v, error = %v", pages, err)
	}
	extracted, err := os.ReadFile(pages[0].File)
	if err != nil || !bytes.Equal(data, extracted) {
		t.Fatalf("small ZIP64 entry bytes changed: %v", err)
	}
}

func TestClassicZIPAllowsMaximumCommentAndEmptyDirectory(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "page", true: "empty"}[empty], func(t *testing.T) {
			input := filepath.Join(t.TempDir(), "comment.cbz")
			var entries []zipEntry
			if !empty {
				entries = append(entries, zipEntry{name: "page.png", data: fixtureBytes(t, "page1.png")})
			}
			writeZip(t, input, entries...)
			data, err := os.ReadFile(input)
			if err != nil {
				t.Fatal(err)
			}
			binary.LittleEndian.PutUint16(data[len(data)-2:], 65535)
			data = append(data, bytes.Repeat([]byte{'x'}, 65535)...)
			if err := os.WriteFile(input, data, 0600); err != nil {
				t.Fatal(err)
			}
			pages, err := comicarchive.Extract(context.Background(), input, t.TempDir(), protocolLimits())
			if err != nil || len(pages) != len(entries) {
				t.Fatalf("maximum ZIP comment: pages = %+v, error = %v", pages, err)
			}
		})
	}
}
