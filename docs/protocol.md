# Comic Pages protocol v1

All routes are relative to the Silo installation's plugin prefix:

```text
/api/v1/plugins/{installation_id}
/api/v2/plugin-content/plugins/{installation_id}
```

The manifest requires authenticated access. Silo authenticates the ordinary
`Authorization` header and supplies `X-Silo-User-Id` to the plugin. Because Silo
does not forward the original bearer token or profile ID, the client also sends
them in a JSON POST body. The plugin checks that the body token identifies the
same account as the trusted host header. The configured upstream Silo URL must
refer to the same server that hosts the plugin.

Account validation uses `/api/v1/auth/me` on v1 and `/api/v2/account/me` on v2.
The v2 route and string account IDs come from the pinned
[Silo v2 OpenAPI contract](https://github.com/Silo-Server/silo-server/blob/0416528027bee67b1fda8e78f06a3d5dfc4e4137/contracts/api/v2/openapi.json).

## Prepare a chapter

`GET /v1/health` needs only the host-authenticated request and no body. It
reports whether the plugin is configured; it does not check archive access.

`POST /v1/pages`, with `Content-Type: application/json`:

```json
{
  "token": "reader-access-token-or-api-key",
  "profile_id": "selected-profile-id",
  "profile_token": "optional-pin-verification-token",
  "content_id": "chapter-content-id",
  "file_id": "archive-file-id",
  "api_version": "v1",
  "offset": 0
}
```

Omit `profile_token` when none is needed. `api_version` is `v1` or `v2` and
selects the upstream Silo API. It is independent of this plugin protocol's
version. Clients cannot supply an upstream URL or a file path.

A ready chapter returns HTTP 200:

```json
{
  "cache_key": "64-character-sha256-cache-identifier",
  "chunk_bytes": 1048576,
  "pages": [
    {"name": "page1.png", "size": 49348},
    {"name": "page2.png", "size": 49348}
  ]
}
```

Page order is natural filename order. Indexes are zero based. Treat `cache_key`
as opaque. HTTP 202 with `{"status":"preparing"}` means a background download
or extraction is still running. The handler briefly waits before returning 202,
so clients can poll sequentially without requiring a timer API. Bound the total
number of polls and display an error when preparation cannot finish.

## Fetch image bytes

`POST /v1/page/{index}` uses the same body, plus the returned `cache_key` and
the desired byte `offset`. Offsets start at zero and advance by 1,048,576 bytes.
The server returns HTTP 200 and at most 1 MiB of binary data. The last response
may be shorter. Join chunks until the page-list size is reached, then decode
the complete image. Check status and exact expected chunk length before joining.

Every chunk request rechecks the reader's chapter and file access, including
cache hits. A stale or missing cache entry returns HTTP 409; prepare the chapter
again instead of joining bytes from different revisions. Do not silently retry
with different credentials or a different profile.

Aidoku places the revision, page index, and profile ID in the page URL to keep
its local image cache separate across revisions and profiles. These query
parameters are cache identifiers. They do not authorize access. The token and
PIN verification token remain in the request body and ordinary auth headers.

## Errors

Errors use a JSON object with an `error` string. HTTP 401 or 403 means the caller
or selected chapter is not authorized. HTTP 409 means the chapter must be
reopened. Invalid archives and configured resource limits are reported as
errors; callers must not try to display the JSON as an image.

Plugin response bodies are buffered through Silo's gRPC proxy. Chunking keeps
large images below the default gRPC receive-message ceiling without changing
the host or recompressing the image.

## Aidoku transport verification

The Aidoku runner preserves a source image request's method and body. Its
[`getImageRequest` implementation](https://github.com/Aidoku/AidokuRunner/blob/cc4d06ff399e7169b9c647bccede7cb29bc805c6/Sources/AidokuRunner/Interpreter.swift#L325-L344)
returns the stored network request as a `URLRequest`.
[`NetRequest.toUrlRequest`](https://github.com/Aidoku/AidokuRunner/blob/cc4d06ff399e7169b9c647bccede7cb29bc805c6/Sources/AidokuRunner/Utilities/NetRequest.swift#L52-L62)
copies both `httpMethod` and `httpBody`. Aidoku passes that complete request to
its image pipeline in
[`ReaderPageView`](https://github.com/Aidoku/Aidoku/blob/73c55ffa685b9edbbd5a269c9a0de18d5dc4f43b/Aidoku/Features/Reader/Page/ReaderPageView.swift#L177-L213).
This source review supports the POST image transport; it is not an iOS device test.
