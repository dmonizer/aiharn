# Configuration subsystem

`internal/config` is the dependency root and must not import other Aiharn
packages. It loads TOML, interpolates `${VAR}` from the environment, resolves
local paths relative to the config file, expands `~` only for local paths,
applies defaults, and performs semantic validation.

Secret-bearing fields such as `api_key` and `password` must be redacted at every
error and logging boundary. Committed `config.toml` files may reference only
environment variables; real credentials belong in git-ignored `*.local.toml`
files.
