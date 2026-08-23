#!/bin/sh
set -eu
exec python3 "$(dirname "$0")/tool.py" test
