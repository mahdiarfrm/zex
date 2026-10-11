#!/bin/sh
# Start Zex: the web UI plus its local relay. Needs Python 3.9+.
cd "$(dirname "$0")" || exit 1
exec python3 server.py --open "$@"
