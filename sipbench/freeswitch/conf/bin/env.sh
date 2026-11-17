#!/bin/sh
# Usage: env.sh NAME DEFAULT
# Prints the environment variable NAME, or DEFAULT when it is unset or empty. Used by vars.xml.
eval "v=\${$1:-}"
if [ -n "$v" ]; then
  printf '%s' "$v"
else
  printf '%s' "$2"
fi
