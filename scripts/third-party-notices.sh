#!/bin/sh
# Writes the licences of everything compiled into the Keera binaries to stdout.
#
# MIT, BSD and Apache-2.0 require their notice to ship with a binary. The list
# comes from the build graph, not go.mod, so test-only modules are left out and
# no new dependency is forgotten.
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

# The Go standard library is linked into every binary. Some toolchains, such
# as Nix's, do not install Go's LICENSE, but the golang.org/x modules carry the
# same text.
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

# The control panel embeds this font, which is not a Go module.
printf '\n%s\nSpace Grotesk, the control panel'\''s display font\n\n' "$rule"
cat internal/webui/fonts/OFL.txt

printf '\n%s\nTrademarks\n\n' "$rule"
cat internal/webui/marks/NOTICE
