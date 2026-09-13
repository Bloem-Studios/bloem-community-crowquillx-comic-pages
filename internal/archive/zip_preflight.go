package archive

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	zipEOCDSize = 22
	zipCDSize   = 46
)

// preflightZIP counts actual central-directory headers before archive/zip
// allocates per-entry metadata. The input must be a bounded, context-aware
// ReaderAt. Memory use here is independent of the advertised entry count.
//
// v0.1 accepts classic, single-disk archives with a directory immediately
// followed by EOCD. ZIP64 EOCD, trailing data, and offset correction for
// self-extracting archives are unsupported. Per-entry ZIP64 extras remain
// supported by archive/zip when the containing archive has a classic EOCD.
func preflightZIP(r io.ReaderAt, size int64, maxEntries int) error {
	if size < zipEOCDSize {
		return fmt.Errorf("%w: missing ZIP EOCD", ErrCorruptArchive)
	}
	tail := make([]byte, min(size, zipEOCDSize+65535))
	if _, err := r.ReadAt(tail, size-int64(len(tail))); err != nil {
		return err
	}
	// Use the last signature, as archive/zip does. Reject ambiguous or
	// truncated comments rather than falling back to a different EOCD.
	end := -1
	for i := len(tail) - zipEOCDSize; i >= 0; i-- {
		if string(tail[i:i+4]) == "PK\x05\x06" {
			end = i
			break
		}
	}
	if end < 0 {
		return fmt.Errorf("%w: missing ZIP EOCD", ErrCorruptArchive)
	}
	eocd := tail[end:]
	u16, u32 := binary.LittleEndian.Uint16, binary.LittleEndian.Uint32
	if zipEOCDSize+int(u16(eocd[20:])) != len(eocd) {
		return fmt.Errorf("%w: invalid ZIP comment length or trailing data", ErrCorruptArchive)
	}
	endOffset := size - int64(len(eocd))
	disk, directoryDisk := u16(eocd[4:]), u16(eocd[6:])
	diskCount, count := u16(eocd[8:]), u16(eocd[10:])
	directorySize, directoryOffset := int64(u32(eocd[12:])), int64(u32(eocd[16:]))
	if count == 0xffff || diskCount == 0xffff || directorySize == 0xffffffff || directoryOffset == 0xffffffff {
		return fmt.Errorf("%w: ZIP64 EOCD", ErrUnsupportedArchive)
	}
	// Also reject a ZIP64 locator when classic fields were not saturated.
	if endOffset >= 20 {
		var signature [4]byte
		if _, err := r.ReadAt(signature[:], endOffset-20); err != nil {
			return err
		}
		if string(signature[:]) == "PK\x06\x07" {
			return fmt.Errorf("%w: ZIP64 EOCD locator", ErrUnsupportedArchive)
		}
	}
	if disk != 0 || directoryDisk != 0 || diskCount != count {
		return fmt.Errorf("%w: multidisk ZIP", ErrUnsupportedArchive)
	}
	if int(count) > maxEntries {
		return ErrEntryLimitExceeded
	}
	if directoryOffset > endOffset || directorySize != endOffset-directoryOffset {
		return fmt.Errorf("%w: invalid ZIP directory offset or size", ErrCorruptArchive)
	}
	var header [zipCDSize]byte
	actual := 0
	for offset := directoryOffset; offset < endOffset; {
		if endOffset-offset < zipCDSize {
			return fmt.Errorf("%w: truncated ZIP directory header", ErrCorruptArchive)
		}
		if _, err := r.ReadAt(header[:], offset); err != nil {
			return err
		}
		if string(header[:4]) != "PK\x01\x02" {
			return fmt.Errorf("%w: invalid ZIP directory header", ErrCorruptArchive)
		}
		actual++
		if actual > maxEntries {
			return ErrEntryLimitExceeded
		}
		if u16(header[34:]) != 0 {
			return fmt.Errorf("%w: multidisk ZIP entry", ErrUnsupportedArchive)
		}
		headerSize := int64(zipCDSize) + int64(u16(header[28:])) + int64(u16(header[30:])) + int64(u16(header[32:]))
		if headerSize > endOffset-offset {
			return fmt.Errorf("%w: ZIP directory fields exceed directory size", ErrCorruptArchive)
		}
		offset += headerSize
	}
	if actual != int(count) {
		return fmt.Errorf("%w: inconsistent ZIP entry count", ErrCorruptArchive)
	}
	return nil
}
