package archive

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"testing"
)

func TestPixelDimensionsWithoutOverflow(t *testing.T) {
	for _, test := range []struct {
		width, height int
		want          error
	}{
		{8192, 8192, nil},
		{1, MaxImagePixels, nil},
		{8193, 8192, ErrImageTooLarge},
		{math.MaxInt, math.MaxInt, ErrImageTooLarge},
		{math.MaxInt, 2, ErrImageTooLarge},
		{0, 1, ErrInvalidImage},
		{1, 0, ErrInvalidImage},
		{-1, 1, ErrInvalidImage},
	} {
		if err := checkImageDimensions(test.width, test.height); !errors.Is(err, test.want) {
			t.Errorf("dimensions %dx%d: %v, want %v", test.width, test.height, err, test.want)
		}
	}
}

type countedDirectory struct {
	io.ReaderAt
	headers int
}

func (r *countedDirectory) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == zipCDSize {
		r.headers++
	}
	return r.ReaderAt.ReadAt(p, off)
}

func TestPreflightCountsActualHeadersAndStopsAtLimit(t *testing.T) {
	// archive/zip accepts a count modulo 65536. This directory advertises
	// one entry but contains 65537. Preflight must stop after header 2049.
	const actual = 65537
	data := make([]byte, actual*zipCDSize+zipEOCDSize)
	for i := 0; i < actual; i++ {
		copy(data[i*zipCDSize:], "PK\x01\x02")
	}
	eocd := data[actual*zipCDSize:]
	copy(eocd, "PK\x05\x06")
	binary.LittleEndian.PutUint16(eocd[8:], 1)
	binary.LittleEndian.PutUint16(eocd[10:], 1)
	binary.LittleEndian.PutUint32(eocd[12:], actual*zipCDSize)
	r := &countedDirectory{ReaderAt: bytes.NewReader(data)}
	if err := preflightZIP(r, int64(len(data)), 2048); !errors.Is(err, ErrEntryLimitExceeded) {
		t.Fatalf("error = %v, want entry limit", err)
	}
	if r.headers != 2049 {
		t.Fatalf("read %d directory headers, want 2049", r.headers)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := preflightZIP(contextReaderAt{ctx: ctx, reader: r}, int64(len(data)), 2048); !errors.Is(err, context.Canceled) {
		t.Fatalf("preflight cancellation = %v", err)
	}
}
