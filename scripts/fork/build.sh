#!/usr/bin/env bash
# Native, identifiable artifacts only. This script never installs or starts services.
set -euo pipefail
cd "$(dirname "$0")/../.."
export CGO_ENABLED=1
export GOMAXPROCS="${GOMAXPROCS:-2}"
fork_sha=$(git rev-parse HEAD)
upstream_tag=v0.26.1
upstream_sha=299e3dbffb5a6827bd89d9037ef8e483e5073bfa
version="${FORK_VERSION:-0.26.1-fork.${fork_sha:0:12}}"
if [[ ! "$version" =~ ^[A-Za-z0-9._-]+$ ]]; then
  echo 'Invalid FORK_VERSION' >&2
  exit 1
fi
if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  echo 'Refusing to label a modified checkout with a clean commit SHA' >&2
  exit 1
fi
platform="$(go env GOOS)_$(go env GOARCH)"
case "$platform" in linux_arm64|darwin_arm64) ;; *) echo "Native ARM64 required: $platform" >&2; exit 1 ;; esac
if [[ "$(go env GOHOSTOS)_$(go env GOHOSTARCH)" != "$platform" ]]; then
  echo 'Cross compilation is not supported by this native build script' >&2
  exit 1
fi
output="${FORK_OUTPUT_DIR:-$PWD/dist/fork/$platform}"
mkdir -p "$output"
build_date=$(date -u +%Y-%m-%dT%H:%M:%SZ)
module=github.com/janekbaraniewski/openusage
go build -p 1 -trimpath -ldflags="-s -w -X $module/internal/version.Version=$version -X $module/internal/version.CommitHash=$fork_sha -X $module/internal/version.BuildDate=$build_date" -o "$output/openusage" ./cmd/openusage
"$output/openusage" --version
file "$output/openusage"
if [[ "$(go env GOOS)" == linux ]]; then
  ldd "$output/openusage" > "$output/runtime-libraries.txt"
else
  otool -L "$output/openusage" > "$output/runtime-libraries.txt"
  otool -l "$output/openusage" > "$output/macho-load-commands.txt"
  xcrun --show-sdk-version > "$output/sdk-version.txt"
fi
(cd "$output" && go version -m openusage) > "$output/go-build.txt"
cc --version > "$output/c-compiler.txt"
go env CC CXX CGO_CFLAGS CGO_LDFLAGS > "$output/cgo-config.txt"
if [[ "$(git rev-parse HEAD)" != "$fork_sha" || -n "$(git status --porcelain --untracked-files=all)" ]]; then
  echo 'Source checkout changed during build; refusing to certify the artifact' >&2
  exit 1
fi
python3 - "$output" "$version" "$fork_sha" "$upstream_tag" "$upstream_sha" "$build_date" "$platform" <<'PY'
import hashlib, json, pathlib, subprocess, sys
out, version, sha, tag, upstream, date, platform = sys.argv[1:]
p = pathlib.Path(out)
metadata = dict(version=version, fork_sha=sha, upstream_base_tag=tag,
                upstream_base_sha=upstream, build_date_utc=date, platform=platform,
                go_version=subprocess.check_output(['go', 'version'], text=True).strip(),
                cgo_enabled=True, signed=False)
registry = pathlib.Path('docs/fork/patches.md')
if registry.exists():
    (p / 'patches.md').write_bytes(registry.read_bytes())
    metadata['patch_registry_sha256'] = hashlib.sha256(registry.read_bytes()).hexdigest()
(p / 'metadata.json').write_text(json.dumps(metadata, indent=2) + '\n')
with (p / 'checksums.txt').open('w') as checksums:
    for f in sorted(p.iterdir()):
        if f.is_file() and f.name != 'checksums.txt':
            checksums.write(hashlib.sha256(f.read_bytes()).hexdigest() + '  ' + f.name + '\n')
PY
printf 'Artifacts: %s\n' "$output"
