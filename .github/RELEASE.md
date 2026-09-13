Comic Pages now removes cached chapters after 30 minutes without access. A
background cleanup runs every minute, including when the server is idle.

The default cache budget is now 2 GiB and includes temporary archive data.
Existing explicit `max_cache_bytes` settings keep their configured value.
Failed deletions retain their budget until cleanup succeeds, preventing later
jobs from treating occupied space as free.

Temporary downloads and partial extraction files are removed after success or
failure. Graceful shutdown clears completed pages. Restart removes crash
leftovers before accepting requests. After a forced stop, cleanup requires a
restart or manual removal of the dedicated cache directory.

[Installation and disk usage](https://github.com/crowquillx/silo-comic-pages#disk-usage-and-automatic-cleanup)

This update works with Aidoku Silo source v5. Linux amd64 and arm64 binaries,
SHA-256 checksums, and dependency license notices are attached. No live server
installation is part of this release.
