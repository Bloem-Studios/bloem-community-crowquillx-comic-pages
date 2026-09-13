# Cache cleanup in v0.1.1

The initial release had size-based eviction and startup cleanup, but no idle
sweep for pages with upstream validators. It also ignored some filesystem
removal errors while releasing the corresponding disk budget. This update adds
idle cleanup and keeps undeleted data charged until it is removed.

## Policy

| Item | Policy |
| --- | --- |
| Default cache budget | 2 GiB, shared by completed images and extraction jobs |
| Configurable maximum | 4 GiB, with existing explicit settings retained |
| Idle lifetime | 30 minutes since the last page or page-list cache access |
| Sweep interval | 1 minute, including when no HTTP requests arrive |
| Temporary archives and failed extraction files | Removed when the job ends |
| Active page reads | Files stay present until the read releases them |
| Failed deletions | Retain the budget and retry instead of admitting more data |
| Graceful shutdown | Cancel/drain work, remove cache files, then release the lock |
| Crash or forced stop | Next startup removes leftovers before accepting work |

These are file-byte limits, with filesystem metadata and allocation rounding
outside the accounting. The executable is about 15 MB. Files are created on
demand; the 2 GiB default is a ceiling rather than a preallocated file.
With default archive limits, job admission needs 1.5 GiB of available cache
budget. Lowering the cache below that requires lower archive/extraction limits.

If removal fails because of permissions or a filesystem problem, disk remains
occupied until that problem is resolved. The plugin retains the charge and
may reject new jobs with `cache_full`. A crashed or uninstalled process cannot
run a timer. Restart it for automatic recovery, or remove its dedicated cache
directory after permanently uninstalling it.

## Verification

Regression tests cover cleanup without incoming requests, idle expiry, active
read protection, deletion failures and retries, temporary files after successful
and failed extraction, and request draining before cache reuse. The binary
integration test inspects the cache directory after a normal SDK shutdown and
a SIGTERM shutdown. Only the ownership marker and lock file may remain.

All local race tests, vet, and six executable integration cases passed. The
stripped amd64 binary was 15,323,298 bytes; arm64 was 14,418,082 bytes. The
integration test measured these retained bytes, excluding the control files:

| Fixture | Downloaded archive bytes | Cached image bytes | Image bytes after shutdown |
| --- | ---: | ---: | ---: |
| RAR5 solid, three generated PNGs | 127,742 | 148,044 | 0 |
| ZIP with a large generated PNG | 767,665 | 3,148,217 | 0 |

The idle-sweep test uses an aged entry and a short timer to exercise the real
background cleanup loop without waiting 30 minutes. Separate tests check the
30-minute last-access boundary and cancellation of a partially extracted job.
The executable shutdown cases use completed jobs. Active-job cancellation is
covered by a backend test. The SDK's
[host shutdown](https://github.com/hashicorp/go-plugin/blob/v1.7.0/client.go#L527-L567)
can force-kill a process after two seconds; that case relies on startup recovery.

Commands used for the release:

```sh
go test -race -count=1 ./...
go vet ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o dist/plugin-linux-amd64 .
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dist/plugin-linux-arm64 .
go run ./cmd/catalog-index manifest.json dist dist/repository.json
COMIC_PAGES_TEST_BINARY="$PWD/dist/plugin-linux-amd64" go test -count=1 -v ./integration
```

This plugin-only update keeps the Aidoku v5 protocol unchanged. No live Silo
installation or source package change is part of this release.
