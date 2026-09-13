# WebP fixture provenance

`generate-webp.py` creates three original 16 by 12 pixel color patterns with
Pillow 12.1.1 and libwebp 1.5.0. The images use this repository's MIT license.
They contain no downloaded artwork.

- `fixtures/pattern-lossless.webp` exercises VP8L.
- `fixtures/pattern-lossy.webp` exercises VP8.
- `fixtures/pattern-alpha.webp` exercises VP8X, ALPH, and VP8.

Run `python3 testdata/generate-webp.py` to regenerate. Regression tests compare
the extracted WebP files byte for byte with these originals. Decoder output
is never re-encoded during extraction. Existing PNG and RAR fixtures are
unchanged.
