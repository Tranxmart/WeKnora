# Tranxmart Document Service data source

The `document_service` connector imports retained raw documents into an existing
**document** knowledge base. Configure the knowledge base and its embedding,
chat, and any required media models with the normal WeKnora administration UI.
New document knowledge bases enable Wiki indexing and automatic page generation
by default, so successfully parsed imports also generate linked Wiki pages when
the required generation model is configured. An administrator can opt out in the
knowledge-base settings. Existing knowledge bases retain their saved indexing
and automatic-generation settings.
An administrator chooses **Tranxmart Documents**, tests the connection, expands
Manual, GitHub and/or Feishu, and selects the folders or documents for that
knowledge base. Whole categories remain selectable. The default is all three
categories, incremental synchronization every five minutes (`0 */5 * * * *`),
overwrite conflicts and explicit-deletion synchronization. Schedule, pause/resume,
full synchronization, logs and conflict strategy use the existing controls.
Selection filters synchronization; it is not an authorization boundary.
The deployment's service identity reads the corpus allowed by Document Service.

## Team knowledge bases and directory selection

Create one document knowledge base per team. In each knowledge base's Data sources
settings, add **Tranxmart Documents** and clear the default whole-category
selections before checking that team's folders. A source can mix folders from
Manual, GitHub and Feishu. Checking a folder includes every document below it,
including nested folders and future additions. Individual document leaves can
also be selected. Save the source and synchronize it using the normal controls.
Editing the source reveals the ancestors of saved nested selections.

The picker reconstructs the complete tree from every page of retained active
documents using `ListDocuments`; it does not download content or call the
person-only directory API. Only folders containing retained documents can appear.
Empty folders become selectable after Document Service retains a document below
them. A listing failure reports an error instead of returning a partial tree.
Feishu document ancestors are shown as branches for their descendants; a retained
ancestor document is also available as its own document leaf.

`resource_ids` accepts category IDs (`manual`, `github`, `feishu`), manual
`dir:<directory UUID>` IDs, `doc:<document UUID>` IDs, and provider directory IDs
`path:<source>:<base64url JSON array of ordered ancestor keys>` (unpadded encoding).
A document is imported when any selection matches. Provider paths are matched by
source and exact ancestor-key prefix, so names and identically named folders in
other trees cannot broaden the selection. Manual UUID selections follow directory
moves; provider paths follow stable keys at the selected ancestry. A move to a
different provider ancestry requires selecting that new location. Labels may
change without changing selection identity. Existing UUID selections remain valid.
Provider path selections list only the necessary categories and filter hierarchies
locally. UUID directory/document selections enumerate all categories because they
do not carry a source type. An empty API selection retains the legacy all-category
meaning; the UI requires at least one selection.

## Deployment prerequisites

Deploy the Document Service API admission from monorepo PR #17129 and the WeKnora
workload identity, reader grant, storage and native OIDC setup from PR #17190
before enabling this connector. The connector introduces no login or workspace
changes. It registers without making network calls, so ordinary installations
start without these settings; connection testing explains missing configuration.

| Environment variable | Value / default |
| --- | --- |
| `IDENTITY_SERVICE_URL` | Required identity service base URL; `/token` is appended |
| `IDENTITY_TOKEN_FILE` | Required rotating projected workload token path |
| `IDENTITY_AUDIENCE` | Required audience of the projected token, as configured by the deployment |
| `DOCUMENT_SERVICE_URL` | `http://document-service.service.svc.cluster.local:8080` |
| `DOCUMENT_SERVICE_RESOURCE` | `https://document-service.txm.internal` |
| `MAX_FILE_SIZE_MB` | Existing upload limit; production uses 50 MiB |

Authentication is managed by the deployment. Do not submit or store credentials
on this data source; credential PUT/DELETE requests are rejected. The current
projected JWT is exchanged for an access token with the Document Service resource.
Tokens refresh at half their reported lifetime, with one exchange/retry after an
unauthenticated RPC. An expired token is never used. Identity and RPC traffic are
restricted to their configured origins and reject redirects. Object capability
GETs use a separate client without bearer credentials and retain WeKnora's
outbound URL protections. No global SSRF allowlist change is required.

## Synchronization and failure recovery

The source uses only `ListDocuments`, `CreateDocumentDownload` and
`BatchGetDocuments`. Downloads select an explicit immutable version, validate the
capability's version metadata, reject redirects and compressed/partial responses,
and check the exact byte count and SHA256 before ingestion. Expired capabilities
are renewed once for the same version. Unsupported, empty or oversized files
produce visible partial-run results, preserve any prior import, and are considered
again on later scans. Configure missing required models and retry a failed run;
existing parse retry controls handle documents accepted for asynchronous parsing.

The existing `last_sync_cursor` JSON stores the inventory, accepted fingerprints,
page token and deletion-check progress. It never stores tokens or capability URLs.
Completed pages checkpoint only after ingestion acknowledges their items. Replays
recognize a stored Document Service fingerprint before replacing an import. The
backend serializes runs for each data source before reading its cursor. This is
an in-process lock: keep PR #17190's **single backend replica and Recreate** rollout
strategy; multiple backend processes require a distributed lock first.

Each completed scan starts its next run at the head of the listing, picking up
items inserted during pagination. Selection changes restart pagination while
retaining the inventory. Changing the configured service origin or resource
requires a new data source. A forced full synchronization downloads and verifies
selected documents again while retaining the previous deletion baseline.

With deletion synchronization enabled, a completed listing followed by an explicit
readable `DELETED` result removes a known import. A known active document listed
outside its directory/document selection (moved out, or the selection narrowed)
is also removed, and re-imported if it moves back. Missing or restricted IDs and
categories not enumerated by the current selection preserve imports. Disabling
deletion synchronization preserves both excluded and explicitly deleted imports.
Tombstone inventory is retained so enabling deletions later still reconciles it.
Document Service remains authoritative; WeKnora stores managed ingestion copies.
Replacement uses the existing asynchronous parser and temporarily removes old
searchable chunks while the replacement is parsed.

Imported metadata records document/version IDs, SHA256 digest, immutable upload
filename, source type/key/revision, ordered hierarchy including stable keys,
source metadata, available original URL and the synchronization fingerprint.
Filenames include the category root and sanitized hierarchy labels. Document
Service observation timestamps are kept separately from upstream edit times.

## Regenerating the client contract

`scripts/generate-txm-documents.sh` pins the canonical monorepo revision
`dbdda22991d48744f991d42906ba71d217cbe7f7`, the proto inputs and generator versions.
Generated protobuf/Connect bindings and their transitive imports are checked in
under `internal/txmdocuments`. Regeneration may require authenticated monorepo
access. Normal Go tests, frontend builds and container builds need no monorepo
checkout or private monorepo credentials.

## Release and deployment verification

Publish immutable full Linux amd64 app, UI and docreader images from the same
fork release revision; record source revision, recipe hashes and verified image
digests in release assets. The monorepo selects those digests in
`txm/kubernetes/weknora/images.libsonnet`. Keep its pinned ParadeDB and Redis
images. `docker/Dockerfile.app` includes the guarded AnyDoc `.llvm*` archive
metadata removal before Go compilation; build with `WITH_ANYDOC=1` and retain the
existing runtime paths.

After both parent PRs are merged and deployed and the reader grant is activated,
verify the UI connection test and nested folder selection in each category,
separate team knowledge bases, designated Markdown/PDF
fixtures, searchability and recorded version provenance, updates, restart recovery
and partial-run reports. Exercise explicit deletion with a controlled fixture or
simulated contract response. These checks require the live deployment; unit tests
use fake transports and repositories and do not activate production grants.
