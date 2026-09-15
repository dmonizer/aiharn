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

exec python3 - "$port" "$script_dir" <<'PY'
import sys
from functools import partial
from http.server import HTTPServer, SimpleHTTPRequestHandler

class NoStore(SimpleHTTPRequestHandler):
    def end_headers(self):
        self.send_header("Cache-Control", "no-store")
        super().end_headers()

port = int(sys.argv[1])
directory = sys.argv[2]
handler = partial(NoStore, directory=directory)
HTTPServer(("0.0.0.0", port), handler).serve_forever()
PY
