# v0.1.0 validation

Validated on 2026-09-13 on Linux amd64 (Intel Core i7-8700). The plugin is a
separate native Silo gRPC process. The Aidoku source remains `no_std` WASM and
uses the plugin only when an installation ID is configured. The existing CBZ
range reader is unchanged.

## Build and licenses

The release build uses Go 1.26.6, `CGO_ENABLED=0`, `-trimpath`, and
`-ldflags='-s -w'`. Local builds used `golang:1.26-alpine` (Go 1.26.6), with GCC
and musl development headers added only to run the race detector. Neither is a
runtime dependency of the release executables.

| Component | Version | License |
| --- | --- | --- |
| Comic Pages | 0.1.0 | [MIT](../LICENSE) |
| RAR decoder | rardecode/v2 2.4.1 | [BSD-2-Clause](https://github.com/nwaples/rardecode/blob/v2.4.1/LICENSE) |
| WebP decoder | golang.org/x/image 0.46.0 | [BSD-3-Clause](https://github.com/golang/image/blob/v0.46.0/LICENSE) |
| Silo SDK | 0.12.0 | [Apache-2.0](https://github.com/Silo-Server/silo-plugin-sdk/blob/v0.12.0/LICENSE) |
| Go runtime and standard library | 1.26.6 | [BSD-3-Clause](https://github.com/golang/go/blob/go1.26.6/LICENSE) |

The complete pinned module graph is in `go.mod`/`go.sum`. License and notice
texts are reproduced in [THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md),
generated with `python3 scripts/third-party-notices.py`. No UnRAR or proprietary
RAR code is linked into the plugin. Fixture generation used rars; independent
UnRAR-based verification was a development-only check.

Pre-publication stripped executable sizes were 15,311,010 bytes for Linux amd64
and 14,418,082 bytes for Linux arm64. The notices file was 391,357 bytes. Release
builds embed Git revision metadata, so the published artifacts and their exact
SHA-256 hashes are recorded in the release's `repository.json` and
`checksums.txt`. The amd64 executable was exercised; arm64 was cross-compiled.

## Verification commands

```sh
gofmt -w internal integration
go test -race -count=1 ./...
go vet ./...
python3 scripts/third-party-notices.py
make build
bin/plugin manifest
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/plugin-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/plugin-linux-arm64 .
go run ./cmd/catalog-index manifest.json dist dist/repository.json
COMIC_PAGES_TEST_BINARY="$PWD/dist/plugin-linux-amd64" go test -count=1 -v ./integration
```

All checks passed. The SDK integration test launches the actual executable,
validates its manifest checksum, configures it over gRPC, and calls its HTTP
route capability. Its six cases cover RAR4/RAR5, normal and solid archives, v2
string IDs and `/account/me`, and a PNG larger than 3 MiB delivered in four
chunks. Every page matches its original bytes. It also checks trusted-host
identity mismatch, authorization after cache population, stale cache revisions,
bodyless health requests, and one archive download per prepared chapter.

Focused unit tests cover bounded image dimensions, WebP variants, ZIP directory
entry-count lies, path/link rejection, extraction limits, request timeouts,
changed upstream validators, extraction deduplication, disk reservation before
download, ownership markers, exclusive cache leases, and cancel/join before
cache cleanup. The final implementation received independent source, extraction,
and backend review; the regression tests retain the defects found during review.
Reconfiguration cancels and drains both page-list and image requests before
reusing a cache directory; a paused-authorization test checks both handlers.

## Runtime measurements

A single local run of the actual stripped child executable produced the table
below. `/usr/bin/time -f %M` recorded maximum RSS in KiB; Python's monotonic
clock measured process launch through successful extraction. Each archive
contains three images. Larger fixtures contain original generated 512 × 768
PNGs, each 1,180,659 bytes, and were checked byte for byte against their originals.
The generator and provenance are linked in [the fixture notes](../testdata/fixtures/README.md).

| Fixture | Archive bytes | Elapsed ms | Peak RSS KiB |
| --- | ---: | ---: | ---: |
| rar40-normal.cbr | 5999 | 7.181 | 15228 |
| rar40-solid.cbr | 4040 | 5.970 | 15100 |
| rar50-normal.cbr | 4815 | 6.079 | 15352 |
| rar50-solid.cbr | 127742 | 9.964 | 15100 |
| large/rar40-normal.cbr | 3542121 | 15.168 | 18172 |
| large/rar40-solid.cbr | 3542121 | 14.577 | 18300 |
| large/rar50-normal.cbr | 3542218 | 16.158 | 18428 |
| large/rar50-solid.cbr | 3541868 | 65.957 | 24188 |

The measurement command for each generated archive was:

```sh
/usr/bin/time -f %M -o time.txt dist/plugin-linux-amd64 extract \
  --input FIXTURE.cbr --output EMPTY_OUTPUT_DIRECTORY \
  --max-entries 2048 --max-page-bytes 33554432 \
  --max-total-bytes 1073741824 --max-archive-bytes 536870912 \
  --max-dictionary-bytes 67108864
```

The gRPC preparation measurements, including localhost upstream requests and
child startup, were 8.6–13.4 ms for the four small RAR fixtures and 22.5 ms for a
767,665-byte ZIP containing a PNG larger than 3 MiB. These are fixture measurements,
not worst-case decoder bounds or network/device benchmarks. They do not establish
performance for large PPMd archives. The first read requires one complete archive
download and extraction on the server; cached reads require authorization calls
and one 1 MiB response per image chunk.

## Operational boundaries

Each job has a 120-second deadline. Its child has a 2 GiB virtual-address-space
limit, core dumps disabled, and a parent-death kill signal. This is process
containment, not a measured 2 GiB RSS ceiling. RAR4 PPMd can allocate beyond the
64 MiB dictionary-window setting; its allocation is bounded by the child process
limit rather than that decoder option. The release supports Linux only.

The [README](../README.md#limits) records archive, output, pixel, entry, cache,
and concurrency limits. The cache reserves maximum input plus maximum output
before downloading. It discards extraction data on restart/reconfiguration and
uses an exclusive per-root lease. Reopen an old chapter after cache eviction.

No plugin was installed on tanmedia, juniper, or another live Silo deployment.
No iOS device test was performed. The request-body bridge was verified from
Aidoku's code and with the WASM test runner; see [the protocol](protocol.md).
The tests use the real Silo SDK process protocol with a mock upstream Silo API,
so they do not claim end-to-end validation of a particular deployed Silo build.
