# Text file tools: implementation notes

Companion to [the development specification](text-file-tools-development.md).
The specification is the agreed behaviour; this page records how it is
implemented, which guarantees hold, and the few clarifications that were needed.

## Status

`read`, `write`, and `edit` are implemented and tested against both execution
backends. Local channels run the backend in-process; SSH channels run the same
core logic through the `aiharn fs-helper` helper on the execution host. The tool
names are part of `tools.Names()`, so `tools = "all"` and explicit tool lists both
select them, and configuration validation accepts them.

## Component map

| Layer | Package | Responsibility |
| --- | --- | --- |
| Contract | `internal/execution/filesystem.go` | `Filesystem`, `FilesystemProvider`, `FileVersion`, and the stable `FSError` codes. |
| Local primitives | `internal/execution/fscore` | Root-confined, handle-relative, no-follow access; bounded reads; exclusive creation; atomic replacement. |
| Local backend | `internal/execution/local` | `Transport.NewFilesystem` adapter (`local:<root>` target). |
| Wire protocol | `internal/execution/fsproto` | Length-prefixed JSON frames, protocol version, `init`/`read`/`create`/`replace`/`ping`/`close`. |
| Helper server | `internal/execution/fshelper`, `cmd/aiharn` | `aiharn fs-helper`: serves the protocol on stdin/stdout over `fscore`. |
| Remote backend | `internal/execution/ssh` | `Transport.NewFilesystem` starts the helper on a dedicated connection (`ssh:<user>@<host>:<port>|<root>` target). |
| Domain | `internal/filetools` | Line model, bounded reads, read receipts, mutation ledger, proposals, previews, error vocabulary. |
| Tools | `internal/tools/files.go` | Schemas, descriptions, approval, JSON results, `FileBackend` seam. |
| Runtime | `internal/app/files.go` | Target resolution, channel generations, limits mapping, process-wide coordination registry. |

Dependencies still point one way: `config → llm → execution → approval →
tools → agent → app`. `filetools` sits between `execution` and `tools` and
imports neither `tools` nor `agent`.

## Behaviour highlights

- Paths resolve against the channel `root`; `root_not_configured` is returned
  when a channel has none. `"."` components are canonicalized, `".."` and
  absolute paths outside the root are rejected with `outside_root`.
- Every component is opened relative to the previously opened directory handle
  with `O_NOFOLLOW`; the leaf that is actually opened is re-validated with
  `fstat`, so a symlink swap, a parent rename, or a sibling-prefix trick cannot
  redirect an operation outside the root.
- Reads are bounded while reading (`maxBytes+1`), validate the whole file as
  UTF-8 without NUL bytes, and never return partial content on failure.
- `write` stages content in the destination directory and publishes with
  `linkat(2)`, which fails with `already_exists` for any existing entry,
  including a dangling symlink. It never creates parent directories.
- `edit` stages content in the destination directory, preserves the permission
  bits and ownership, re-checks the expected version immediately before
  `renameat(2)`, and rejects multiply linked files. Failures never truncate.
- Read receipts are mined only after a complete response is prepared, including
  the model-visible result budget. They are opaque, random, and scoped to the
  conversation runtime instance, the agent instance, the channel and root, the
  canonical path, and the exact range.
- A tracked edit invalidates every receipt whose covered section ends at or
  after the mutation start (`mutation.start <= receipt.end`), including its own
  authorizing receipt. Surviving receipts keep their original coordinates.
- Any detected untracked modification (version mismatch: device, inode, size,
  mtime, ctime, or content digest) invalidates every receipt for that file,
  whichever conversation or agent minted it.
- `write` and `edit` validate, prompt, then revalidate the channel generation,
  root, path, version, receipt, and limits before publishing. A channel switch
  during approval — even away and back — fails with `channel_changed`.

## Supported platforms and helper

- The confined local backend is Linux-only. On other platforms
  `NewFilesystem` reports `unsupported_filesystem`; the tools do not fall back to
  a weaker check.
- SSH channels need an `aiharn` binary of the same protocol version on the
  execution host (`fsproto.ProtocolVersion`). The channel's `fs_helper` selects
  the command line; the default is `aiharn fs-helper`. A helper that cannot be
  started, speaks another protocol version, or fails the handshake is reported as
  `unsupported_filesystem` with installation guidance.
- One protocol frame is capped at 16 MiB and the helper's file-exchange cap is
  10 MiB, matching the default `max_file_bytes`. A larger remote file fails with
  `file_limit_exceeded`; nothing is silently truncated.
- A disconnect during a mutation is reported as `outcome_unknown`: the change may
  or may not have been published, receipts for the file are dropped, and the
  model is told to read the file before retrying. Rollback is never claimed.
- The helper command line is static configuration, never model input, and file
  names or contents are never interpolated into a shell command.

## Concurrency model and residual races

Aiharn serializes its own file mutations per execution target. The process-wide
`filetools.Registry` keys coordination by `Filesystem.Target()` (backend kind,
host, and canonical root), so every agent, every conversation runtime, and every
channel alias in the process that resolves to the same target shares one mutex,
one version ledger, and one receipt store. That gives the following guarantees
inside one Aiharn process:

- Mutations serialize; a proposal never publishes a diff it did not preview.
- A mutation re-validates the expected version immediately before publication.
- Tracked mutations invalidate the receipts they should; surviving receipts stay
  valid because invalidation is by mutation position, not by whole-file hash.
- Untracked changes are detected and invalidate every receipt for that file.

Deliberately not guaranteed, and documented for operators:

- **External writers.** An ordinary filesystem offers no compare-and-swap, so an
  external writer can change the file between the final version check and
  `renameat(2)`; the last writer wins. Do not share a channel root with another
  writer when lost updates matter.
- **Other Aiharn processes.** A second process has its own registry, so its
  mutations look like untracked external changes: they invalidate receipts and
  are detected, but they are not serialized against this process.
- **Identical-byte rewrites.** Detection compares device, inode, size, mtime and
  ctime as well as a SHA-256 of the content. A rewrite that preserves every one of
  those (possible on filesystems with coarse timestamps and in-place writes) is
  undetectable; the failure mode is a receipt that stays valid for identical
  content, which cannot change what an edit writes.
- **Power-loss durability.** Atomic visibility is not durability; publication
  fsyncs the file and the destination directory but makes no stronger promise.
- **Sandboxing.** These tools constrain only their own access. `execute_command`
  can still reach paths outside the root.

## Clarifications of the specification

1. **Receipt coverage.** A replacement requires one receipt *covering* the
   replaced range (`receipt.start <= start` and `receipt.end >= end`), and an
   insertion requires one receipt covering the required context from the
   specification's table. Receipts are never combined, because `edit` accepts a
   single `read_id`.
2. **Model-visible result budget.** The read tool refuses a range whose encoded
   JSON envelope would exceed `limits.tool_result_bytes` with
   `read_limit_exceeded`; it does so before minting a receipt, so no receipt can
   authorize content the agent truncated. `[file_tools] max_read_bytes` remains
   the content limit measured before JSON escaping, as specified.
3. **Early validation for `write`.** `write` reports `already_exists` or
   `parent_not_found` before prompting, so the user is never asked to approve a
   creation that cannot succeed. Publication re-checks atomically; the early
   check is a courtesy, not the guarantee.
4. **`.` components.** Accepted and canonicalized rather than rejected. `..` is
   always rejected, including a configured root that contains one.
5. **Remote size cap.** The helper exchanges at most 10 MiB of content per
   request, independent of a larger configured `max_file_bytes`.
6. **Approval prompts.** The preview (creation preview or line diff) travels in
   `approval.Request.Command`; `Args` carries only the filename, coordinates, and
   byte/line counts, never the content, so approval logs do not leak file
   contents.

## Error vocabulary

| Code | Emitted by | Meaning |
| --- | --- | --- |
| `root_not_configured` | filetools | The selected channel has no `root`. |
| `outside_root` | backend | Empty name, absolute path outside the root, or a `..` component. |
| `symlink_not_allowed` | backend | A symlink in any component, including the root. |
| `not_regular_file` | backend | Directory, pipe, socket, or device. |
| `not_found`, `parent_not_found` | backend | Missing file or missing parent directory. |
| `already_exists` | backend | Any entry already occupies a write destination. |
| `invalid_text`, `mixed_newlines` | filetools | Invalid UTF-8/NUL content, or an edit of a file mixing LF and CRLF. |
| `invalid_range` | filetools | Coordinates outside the file, inverted, or negative. |
| `read_limit_exceeded` | filetools | Line cap, byte cap, or result budget exceeded. |
| `file_limit_exceeded` | backend/filetools | Existing file, write content, or edit result above its limit. |
| `read_required`, `read_scope_mismatch` | filetools | Missing, forged, foreign, or insufficiently scoped receipt. |
| `stale_read` | filetools | The file changed after the read; read again. |
| `channel_changed` | filetools | The channel switched while approval was pending. |
| `hard_links_not_allowed` | backend | Multiply linked edit target. |
| `permission_denied`, `io_error` | backend | Host failure before publication. |
| `approval_denied`, `cancelled` | tools | No publication occurred. |
| `unsupported_filesystem` | backend | Platform or helper cannot uphold the contract. |
| `outcome_unknown` | backend | A mutation's commit state is unknown; read before retrying. |

## Test coverage

The contract suite in `internal/execution/fstest` is run unchanged against the
local backend (`internal/execution/local`) and the SSH backend over the
in-process harness (`internal/execution/ssh`). Confinement races, staging
cleanup, hard links, and metadata preservation are covered in
`internal/execution/fscore`. Domain rules — line model, limits, receipt
invalidation, channel generations, unknown outcomes, previews — are covered in
`internal/filetools`. Approval flow, schemas, and result encoding are covered in
`internal/tools/files_test.go`, and whole-turn behaviour through real
configuration, a local channel, and a scripted model in
`internal/app/filetools_integration_test.go`.

Run `make build`, `make vet`, `make test`, and `make race` before shipping a
change in this area.
