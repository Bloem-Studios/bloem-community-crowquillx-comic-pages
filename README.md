# Silo Comic Pages

A Silo plugin that extracts CBR/RAR and CBZ/ZIP archives on the server and
serves individual comic pages. It is designed for
[the Silo Aidoku source](https://github.com/crowquillx/aidoku-silo-sources).
No Silo or Aidoku app changes are required.

The plugin uses the reader's existing Silo credentials to fetch the archive
through Silo's ebook API. It needs no media filesystem mappings, administrator
API key, proprietary `unrar` executable, or separate HTTP port.

## Install

Download the binary for your Silo server from
[Releases](https://github.com/crowquillx/silo-comic-pages/releases).
Linux amd64 and Linux arm64 builds are published with SHA-256
checksums and license notices.

In Silo, open **Administration → Plugins** and upload the binary. Alternatively,
add this repository index to the plugin catalog and install Comic Pages:

```text
https://github.com/crowquillx/silo-comic-pages/releases/latest/download/repository.json
```

Configure the plugin with a Silo API base URL reachable from its process and a
dedicated writable cache directory. For a plugin running inside the Silo
container, the base URL can be `http://127.0.0.1:8090`. Use a dedicated cache
directory with enough free disk space. Cached pages are discarded when the
plugin starts or reconfigures. Do not use a media library directory as the cache
directory.

Use a separate cache directory for each installation. The plugin rejects
nonempty directories it does not own and locks its cache against concurrent
use. Before downloading, it reserves space for the configured maximum archive
plus maximum extracted bytes: 1.5 GiB with the defaults. If you lower the cache
limit below that sum, lower the corresponding archive or extraction limit too.

Update the Aidoku Silo source to v5 or newer. Enter the plugin's installation ID
from Silo's Installed tab in **Reading → Comic Pages plugin installation ID**.
This is the installation ID, not the plugin ID `dev.crowquillx.comic-pages`.
CBR chapters then use the plugin. CBZ chapters keep Aidoku's existing ZIP range
reader. Leaving the field blank keeps the built-in CBR decoder.

## How reading works

1. Aidoku sends the chapter and file IDs, current token, and selected profile
   in an authenticated POST body to the plugin route on the same Silo server.
2. The plugin checks the caller against Silo and checks access to that chapter
   and file. It downloads and extracts an uncached archive in a background job.
3. Aidoku receives a naturally sorted page list. Each page request checks access
   again before returning bytes from the extraction cache.

The first chapter open can take longer while extraction runs. Requests report
that work is in progress instead of holding Silo's 10-second plugin call open.
Aidoku polls for completion. Large images arrive in 1 MiB chunks because plugin
responses are buffered through gRPC. Aidoku joins them without recompression.

Tokens are sent in POST bodies, never in page URLs. They are not written into
the extraction cache. The configured Silo server is the only archive origin;
clients cannot supply arbitrary URLs or filesystem paths. Cached images remain
subject to Silo access checks. Restarting the plugin or eviction can invalidate
an open chapter; reopen it to prepare a fresh page list.

## Limits

| Resource | Default and maximum |
| --- | --- |
| Compressed archive | 512 MiB |
| Decoded entry or image | 32 MiB |
| Decoded image pixels | 67,108,864 |
| Decoded bytes across all entries | 1 GiB |
| Archive entries | 2,048 |
| RAR dictionary window | 64 MiB |
| Extraction cache | 4 GiB |
| Concurrent extraction jobs | 1 |
| Download and extraction job deadline | 120 seconds |
| Page response chunk | 1 MiB |

The connection settings accept lower byte and entry limits. Each extraction
runs in a child process that the plugin can terminate. The child receives a
2 GiB virtual-address-space limit and has core dumps disabled. These limits
do not describe a fixed RSS budget. In particular, the RAR4 PPMd decoder can
allocate a model beyond its dictionary-window setting. The initial release
supports Linux because this process limit has been implemented there.

Supported page formats are PNG, JPEG, GIF, and static WebP. Animated WebP,
encrypted archives, split archives, and filesystem links return errors. ZIP64
end-of-directory records are unsupported; small ZIP archives containing ZIP64
entry extras are accepted. Original archive files are never modified.

## Development

Go 1.26 is required. The plugin is pure Go and builds with `CGO_ENABLED=0`.

```sh
go mod download
go test ./...
go vet ./...
make build
bin/plugin manifest
```

`internal/archive` owns extraction and image ordering. The server and Silo API
adapter own authorization, background work, and cache lifecycle. Synthetic
RAR4/RAR5 normal and solid fixtures contain original generated PNGs; provenance
is recorded in [testdata/fixtures/README.md](testdata/fixtures/README.md).
See [the protocol](docs/protocol.md) and [release validation](docs/validation.md)
for the request contract, measured artifacts, and test coverage.

## License

MIT. RAR extraction uses `github.com/nwaples/rardecode/v2` v2.4.1 under
BSD-2-Clause. The Silo plugin SDK v0.12.0 uses Apache-2.0, and WebP decoding uses
`golang.org/x/image` v0.46.0 under BSD-3-Clause. The release includes
[the dependency license texts](THIRD_PARTY_NOTICES.md).
