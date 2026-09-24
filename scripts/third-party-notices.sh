#!/bin/sh
# Writes the licences of everything compiled into the Keera binaries to stdout.
#
# MIT, BSD and Apache-2.0 all ask for their notice to travel with a binary, and
# a FROM scratch image or a release archive has nothing else to carry it. The
# list comes from the build graph rather than go.mod, so a module only the tests
# use is left out and a new dependency cannot be forgotten.
#
# Run from the repository root, after `go mod vendor`.
set -eu

GO=${GO:-go}
export LC_ALL=C
rule='================================================================'

cat <<'EOF'
Third-party software in Keera

The Keera binaries include the software below. Each part keeps its own licence,
which is reproduced here. The Keera Community Licence does not cover it.
EOF

# The Go standard library and runtime are linked into every Go binary. Some
# toolchains, Nix's among them, do not install Go's LICENSE. The golang.org/x
# modules carry the same text, byte for byte.
golicence=
for f in "$($GO env GOROOT)/LICENSE" vendor/golang.org/x/*/LICENSE; do
  if [ -f "$f" ]; then golicence=$f; break; fi
done
[ -n "$golicence" ] || { echo "notices: found no licence for Go itself" >&2; exit 1; }

printf '\n%s\nThe Go standard library and runtime, %s\n\n' "$rule" "$($GO env GOVERSION)"
cat "$golicence"

$GO list -mod=vendor -deps \
  -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}' ./cmd/... |
  sort -u |
  while read -r path version; do
    files=$(find "vendor/$path" -type f ! -name '*.go' \( \
      -iname 'LICEN[CS]E*' -o -iname 'COPYING*' -o -iname 'NOTICE*' -o -iname 'PATENTS*' \) | sort)
    [ -n "$files" ] || { echo "notices: $path has no licence file" >&2; exit 1; }
    printf '\n%s\n%s %s\n' "$rule" "$path" "$version"
    for f in $files; do
      printf '\n--- %s\n\n' "${f#"vendor/$path/"}"
      cat "$f"
    done
  done

printf '\n%s\nTrademarks\n\n' "$rule"
cat internal/webui/marks/NOTICE
