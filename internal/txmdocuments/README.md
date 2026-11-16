The client exposes only `ListDocuments`, `BatchGetDocuments` and `Download`.
Construction validates environment configuration without reading credentials or
contacting services. Each exchange rereads the projected JWT, requests the
Document Service resource at identity's `/token` endpoint, and caches the issued
access token in memory until its half-life. A refresh failure can reuse an
unexpired access token; rejected or expired credentials cannot fall back to the
projected JWT. `IDENTITY_AUDIENCE` documents the deployed projection's audience,
while `DOCUMENT_SERVICE_RESOURCE` selects the exchange target.

Identity and RPC clients accept requests only to their configured origin and
exact allowed paths, ignore proxy environment variables, never follow redirects,
and have no cookie jars. Signed download capabilities use the application's
SSRF-safe HTTP client, carry no identity headers or cookies, cannot redirect,
and must return the listed immutable length and SHA256 digest.

`proto/` contains the unmodified canonical Document Service API, models, and
all transitive Tranxmart auth protobuf imports from:

- Repository: `Tranxmart/tranxmart`
- Revision: `dbdda22991d48744f991d42906ba71d217cbe7f7` (#17129)
- Source checksums: `proto/SHA256SUMS`

The complete generated protobuf and Connect bindings are checked into `gen/`.
Imports are mapped to this repository; builds require no private Go dependency.
The protobuf `go_package` descriptor metadata remains canonical.

Run `scripts/generate-txm-documents.sh` to regenerate from the checked-in sources.
It requires **protoc 33.5**, and installs pinned **protoc-gen-go v1.36.11** and
**protoc-gen-connect-go v1.18.1**. Existing matching tools can be supplied with
`PROTOC`, `PROTOC_GEN_GO` and `PROTOC_GEN_CONNECT_GO`; use `PROTOC_INCLUDE` if the
well-known protobuf sources are not installed alongside protoc. The optional
`--fetch-sources` flag refreshes the pinned source files using authenticated `gh
api` and verifies their checksums before regeneration. Ordinary regeneration does
not require private repository access.
