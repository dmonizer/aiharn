# Configuration subsystem

`internal/config` is the dependency root and must not import other Aiharn
packages. It loads TOML, interpolates `${VAR}` from the environment, resolves
local paths relative to the config file, expands `~` only for local paths,
applies defaults, and performs semantic validation.

Secret-bearing fields such as `api_key` and `password` must be redacted at every
error and logging boundary. Committed `config.toml` files may reference only
environment variables; real credentials belong in git-ignored `*.local.toml`
files.

## Text file tools

`[file_tools]` (`FileToolsConfig`) carries the `read`/`write`/`edit` limits:
`max_read_lines`, `max_read_bytes`, `max_write_bytes`, `max_file_bytes`, and
`max_edit_result_bytes`. Zero means "use the default" (see `DefaultFileTools`),
like `[limits]`; `Validate` rejects negative values and the two combinations
that make the tools unusable (`max_read_bytes > max_file_bytes`,
`max_write_bytes > max_edit_result_bytes`).

`ChannelConfig.FSHelper` (`fs_helper`) is an optional, statically configured
command line that starts the filesystem helper on an SSH execution host; it is
env-interpolated and rejected if it contains NUL, CR, or LF. An empty value means
`aiharn fs-helper`. It is configuration, never model input, so it is not subject
to filename/content quoting rules.
