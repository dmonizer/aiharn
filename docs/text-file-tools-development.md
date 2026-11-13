# Text file tools: development specification

Status: agreed behavior; implementation proposal. No tools are implemented by this document.

Implementation: implemented for local and SSH channels. See
[implementation notes](text-file-tools-implementation.md) for the component map,
supported platforms, concurrency guarantees, and the clarifications this
specification needed.

## Purpose and scope

Add `read`, `write`, and `edit` tools for text files on the agent's selected
execution channel. Local and SSH channels must expose identical contracts.
All operations are confined to the channel's configured `root`. Editing requires
a successful read by the same agent in the same conversation of the affected
section. Reading via `execute_command` does not qualify.

This is file-tool confinement, not a process sandbox. Existing shell commands
can access paths outside the root; restricting `execute_command` is outside scope.

## Current integration points

- `internal/config/config.go`: `ChannelConfig.Root` is optional today and refers
  to an absolute directory on the execution host. Keep existing channels valid;
  these tools fail when their selected channel has no root.
- `internal/execution/session.go` and `transport.go`: the common seam currently
  exposes shell execution and lifecycle, without structured filesystem methods.
- `internal/tools/tools.go`: schemas, registry, and the built-in names used by
  configuration validation. Add all three names here.
- `internal/approval/gate.go`: existing `ask` and `allow-all` modes; tools must
  explicitly invoke the gate.
- `internal/app`: owns runtime assembly, conversation lifecycle, channel selection,
  and tool construction. Top-level channel selection can change dynamically.
- `internal/app/directory.go`: existing directory checks are not sufficient for
  race-resistant file access. Do not reuse lexical containment as the sole check.

Preserve the documented dependency direction. Filesystem transport mechanics
belong in `execution`; schemas and model-facing errors belong in `tools`;
conversation ownership and channel selection belong in `app`. Do not import
`agent` into `tools`.

## Configuration and limits

Proposed global configuration section (names are implementation guidance):

```toml
[file_tools]
max_read_lines = 500
max_read_bytes = 262144          # 256 KiB
max_write_bytes = 1048576        # 1 MiB
max_file_bytes = 10485760        # 10 MiB, existing file
max_edit_result_bytes = 10485760 # 10 MiB
```

All limits are configurable positive integers, validated at startup. Measure
bytes as encoded UTF-8 content, including newline bytes, before JSON escaping.
The read byte cap applies to returned file content; metadata has a separate
bounded overhead. Never silently truncate tool results. Existing files exceeding
`max_file_bytes` cannot be read or edited, even in small chunks. Bound loading
while reading, rather than trusting a pre-read size check alone.

## Common path and text contract

- Relative filenames resolve against the configured channel root, never the
  current working directory. Absolute filenames are accepted only within root.
- Fail with `root_not_configured` when root is absent; no fallback directory.
- Resolve paths on the execution host. Do not interpret SSH filenames locally.
- Reject escapes, symlinks (including parent components and the configured root),
  directories, pipes, sockets, and devices. Only regular files qualify.
- Use component-aware containment and no-follow, directory-handle-relative access.
  Validate the actual object opened; string-prefix checks and check-then-open are
  insufficient. Never shell-interpolate filenames or content.
- Files and supplied content must be valid UTF-8 with no NUL bytes. Validate the
  whole bounded existing file, not only the requested range.
- Paths are literal, without shell expansion, URL decoding, or globbing.
- Errors must reach the LLM as tool failures with a stable code and corrective
  guidance. Do not return successful-looking partial content on failure.

## Line model

Line indices are zero-based; ranges are start-inclusive and end-exclusive.
LF and CRLF delimit lines. A trailing delimiter terminates the last line and
creates no additional empty line. Empty files have zero lines. Thus `a\n` has
one line, `a\nb` has two, and `\n` has one empty line.

Reads preserve original bytes. Edits preserve the file's LF or CRLF style and
reject mixed LF/CRLF files with `mixed_newlines`. For files without a delimiter,
use LF for newly introduced boundaries. Treat a lone CR as text, not a delimiter.
Write preserves supplied content exactly after validation.

## read

```text
read(filename: string, start_line: int = 0, end_line?: int)
```

If omitted, `end_line = min(file_line_count, start_line + max_read_lines)`.
An explicit end is not clipped to the line cap or EOF. Require:

```text
0 <= start_line <= end_line <= file_line_count
end_line - start_line <= max_read_lines
```

An explicit oversized range fails with `read_limit_exceeded`. If the selected
content exceeds the byte cap, fail and request a smaller range, even when the end
was omitted. A single line exceeding the byte cap cannot be read by this API.
An empty range is valid but normally grants no edit context. Reading `[0, 0)`
of an empty file grants insertion at zero.

Return a structured result encoded using the existing textual tool-result seam:

```json
{
  "filename": "src/example.go",
  "start_line": 0,
  "end_line": 100,
  "file_line_count": 740,
  "content": "...",
  "read_id": "opaque-receipt",
  "has_more": true,
  "next_start_line": 100
}
```

Omit `next_start_line` at EOF. Do not add line-number prefixes to `content`.
Metadata supplies coordinates. Issue a receipt only after a successful complete
read response has been prepared, without transport/output truncation.

## write

```text
write(filename: string, content: string)
```

Create a new regular file; the parent directory must already exist. Fail if any
entry already occupies the destination, including a dangling symlink. Never
create parent directories or overwrite, including during a concurrent creation.
Validate content and `max_write_bytes` before approval/publication. Empty content
is valid. Use ordinary creation permissions subject to the execution host's umask;
do not introduce executable bits implicitly.

Return filename, written byte count, and line count. A successful write does not
issue a receipt and does not satisfy read-before-edit.

## edit

```text
edit(filename: string, read_id: string,
     start_line: int, end_line: int, content: string)
```

Replace complete lines in `[start_line, end_line)`. Equal endpoints insert;
empty content deletes a nonempty selection. Validate coordinates against the
current file and validate the resulting text and byte size before publication.

For a nonempty selection, one receipt must cover the entire selected range.
Receipts cannot be combined to authorize a larger replacement.
For insertion at boundary `p` in a file containing `N` lines:

| Position | Required read context |
| --- | --- |
| `0 < p < N` | Both adjacent lines: `[p-1, p+1)` |
| Prepend, `p = 0`, `N > 0` | First line: `[0, 1)` |
| Append, `p = N`, `N > 0` | Last line: `[N-1, N)` |
| Empty file, `p = N = 0` | Successful read of that empty file |

Normalize supplied LF/CRLF separators to the existing file style. When nonempty
inserted/replacement content precedes retained lines, ensure a terminating
separator. When appending after an unterminated last line, add a separator before
nonempty inserted content. At EOF the supplied content controls the final newline;
otherwise preserve unaffected bytes, including the existing final newline.
Deletion inserts no content and no extra blank line.

Reject editing files with multiple hard links. Preserve permissions on atomic
replacement. Implementations must also account for ownership and access-control
metadata; if these cannot be preserved safely, reject rather than silently change
access. Return filename, resulting byte/line counts, and the replaced range.
Do not return a new receipt: returned mutation metadata is not a read.

## Read receipts and invalidation

Receipts are opaque, server-owned records scoped to:

- Conversation runtime instance, not merely its reusable public session ID.
- Agent instance, not merely a potentially reused name.
- Execution channel identity, root identity, and canonical root-relative path.
- Observed file identity/version and the exact range returned.

Maintain expected full-file versions and a mutation history for tool-tracked edits.
A whole-file hash alone cannot determine receipt validity: legitimate edits after
a read range may change that hash without invalidating the receipt.

For a receipt covering `[s, e)`, invalidate it when a tracked mutation starts at
or before its covered section's end. Conservatively use `mutation.start_line <= e`:
insertion exactly at its end boundary invalidates it too. Preserve it only when
`mutation.start_line > e`. The rule applies to the entire read range, not merely
the next intended edit point. Do not shift receipt coordinates.

Example: after reading `[100, 120)`, a tracked edit starting at 150 preserves the
receipt; changes at 100, 110, or before 100 invalidate it. A change at boundary 120
also invalidates it. Every successful edit invalidates its own authorizing receipt.
Receipts for earlier sections can survive. All affected agents' receipts must be
updated, while ownership still prevents sharing receipts between agents.

Detect untracked modifications using actual file identity, content/version checks,
and available metadata. Any detected untracked modification invalidates all
receipts for that file, regardless of where it occurred. This includes changes
made through `execute_command`, external applications, or an uncoordinated Aiharn
process. Do not attempt to infer a safe external change using a diff. Identical-byte
external rewrites may be undetectable on some filesystems; document this limitation.

Clear receipts when the conversation ends, is cleared/replaced, or the owning
agent is closed. Do not persist them across process restart. An ordinary turn end
or cancellation does not itself end the conversation. A receipt never authorizes
access through a different channel or root.

## Approval and transaction sequence

`read` runs without approval. `write` and `edit` use the existing gate: prompt in
`ask`, proceed automatically in `allow-all`. Preserve existing permission rules
for changing modes.

1. Bind the selected channel/root and its selection generation; validate inputs.
2. Read/validate the target and receipt as needed. Construct the exact proposed
   result and an approval preview (diff for edit, creation preview for write).
3. Request approval without holding a file lock while waiting for a human.
4. After approval, reacquire coordination and revalidate channel generation, root,
   path/object identity, file version, receipt, and all limits. Reject any channel
   switch while approval was pending, even if it switches back.
5. Publish the approved result atomically and update the mutation ledger under the
   same coordination. Never publish a different diff using the old approval.
6. Return success only after publication is known to have succeeded.

Before publication, denial, cancellation, validation errors, and staging failures
leave the destination unchanged and clean up temporary files. Stage new content
in the destination filesystem. Write needs atomic no-replace publication; edit
needs atomic replacement. Never truncate a destination in place.

An SSH disconnect after publication can leave the caller uncertain whether a
mutation committed. Return a distinct `outcome_unknown`, invalidate receipts, and
instruct the LLM to read before retrying. Do not claim rollback or blindly retry.
Likewise cancellation after commit must not be reported as an unchanged-file
failure. Atomic visibility is not a promise of power-loss durability.

## Execution backend and concurrency requirements

Introduce a structured filesystem capability in `internal/execution`, separate
from model tool schemas. It must encapsulate safe reads, version validation,
exclusive creation, atomic replacement, metadata preservation, and confinement.
The runtime supplies a channel-bound backend to the tools and owns the receipt
ledger. Coordinate mutations and receipts across agents and conversation runtimes
within the process for the same execution target; if target aliases cannot be
unified, treat changes through an alias as untracked.

Implement the capability locally and through a remote execution-host helper for
SSH, sharing core filesystem logic where practical. Use a framed, bounded protocol
with structured/encoded arguments. Do not implement remote edits as interpolated
`sed`, `echo`, or shell redirections. Helper availability and supported host
capabilities must be checked; return `unsupported_filesystem` when safety
requirements cannot be met rather than weakening the contract. Document helper
installation/versioning as part of implementation.

The implementation must prove confinement against symlink/path replacement races,
not merely static traversal. Directory-handle traversal and host-specific safe
resolution primitives are needed; account for parent-directory renames as well.

Serialize Aiharn mutations per target and compare the actual expected file version
immediately before publication. Ordinary advisory locks plus atomic rename do not
provide compare-and-swap against arbitrary external writers. There is a residual
external-write race between validation and replacement on ordinary filesystems.
Do not claim unconditional lost-update prevention. Before shipping, document the
supported concurrency model and either enforce the necessary exclusive-writer
condition or reject environments requiring guarantees the backend cannot provide.
This is an implementation feasibility requirement, not permission to silently
weaken read-before-edit or root confinement.

## Error vocabulary

Use stable machine-readable codes with concise messages and recovery hints:

| Code | Meaning / recovery |
| --- | --- |
| `root_not_configured` | Configure the selected channel's root. |
| `outside_root` | Choose a path inside that root. |
| `symlink_not_allowed` | Use a direct regular-file path. |
| `not_regular_file` | Select a text file, not a directory/device/etc. |
| `not_found`, `parent_not_found` | Correct the path; write does not create parents. |
| `already_exists` | Read and edit instead of overwriting. |
| `invalid_text`, `mixed_newlines` | Unsupported encoding/content or edit newline style. |
| `invalid_range` | Supply valid zero-based, end-exclusive coordinates. |
| `read_limit_exceeded` | Request fewer lines/bytes; include effective limits. |
| `file_limit_exceeded` | Existing file or proposed result exceeds its size limit. |
| `read_required`, `read_scope_mismatch` | Read the required section as this agent. |
| `stale_read` | Read again after a conflicting/untracked modification. |
| `channel_changed` | Retry against the intended channel with a fresh read. |
| `hard_links_not_allowed` | Editing this multiply linked file is unsupported. |
| `approval_denied`, `cancelled` | No publication occurred. |
| `permission_denied`, `io_error` | Operation failed before publication. |
| `unsupported_filesystem` | Backend cannot uphold the required safety contract. |
| `outcome_unknown` | Inspect current state before retrying; do not assume rollback. |

Do not expose secrets or unrelated file content in diagnostics. Internal failures
must not mint receipts or advance the successful mutation history.

## Implementation sequence

1. Add limits/defaults and tool names; define shared line/text/error contracts.
2. Build and test execution-host filesystem primitives and local/SSH capability
   negotiation, including atomic publication and confinement feasibility.
3. Add runtime-scoped receipt ownership, target coordination, and mutation history.
4. Implement tool schemas, bounded structured results, approval previews, and
   post-approval validation. Wire runtime construction and agent tool selection.
5. Integrate lifecycle cleanup and channel generation checks. Document configuration,
   helper requirements, supported platforms/filesystems, and concurrency limitations.
6. Complete the acceptance matrix below before advertising the tools as supported.

## Acceptance criteria and validation

Run the same contract suite against local and SSH backends. Use the in-process
SSH harness for transport coverage, and tool/runtime fakes for policy tests.

- Read default first/subsequent chunks; explicit cap violations; byte caps; oversized
  single lines; EOF, empty file/range, negative/reversed/out-of-bounds coordinates;
  UTF-8 multibyte accounting; invalid text beyond the requested section.
- Write empty/new text; existing file/dangling symlink; missing parent; concurrent
  destination creation; permissions; byte-limit boundaries; failed staging cleanup.
- Edit replacement/deletion/interior insertion/prepend/append/empty-file insertion;
  required adjacent context; unterminated last line; LF/CRLF/mixed newline cases;
  exact result-size boundaries; permissions and hard links.
- Missing/forged/cross-agent/cross-conversation/cross-channel receipts; command reads
  not qualifying; write not qualifying; no receipt combination; stale version;
  changes before/inside/at-end/strictly-after range; multiple surviving receipts;
  external changes; clear/restart/agent closure; ordinary turn continuation.
- Missing root; absolute/relative paths; traversal and sibling-prefix escapes;
  symlinked root/parents/leaf; non-regular files; concurrent symlink and directory
  replacement/rename attempts; filenames/content containing shell metacharacters.
- Approval denial/cancellation; mutation and channel switch while awaiting approval;
  switch away and back; `allow-all`; preview matching the committed result.
- Concurrent agents/conversations; publication failures; SSH disconnect before/after
  commit; unknown outcomes; truncated remote protocol; unsupported host capability.

Run `make build`, `make vet`, `make test`, and `make race` for implementation.
A documentation-only change requires Markdown/link inspection and
`git diff --check`; it does not require running the Go suite.
