#!/bin/sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
port=${1:-${PORT:-8080}}

case "$port" in
	""|*[!0-9]*)
		echo "usage: $0 [port]" >&2
		exit 2
		;;
esac

if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
	echo "serve.sh: port must be between 1 and 65535" >&2
	exit 2
fi

exec python3 -m http.server "$port" --bind 0.0.0.0 --directory "$script_dir"
