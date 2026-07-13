#!/bin/sh
set -eu

version=${1:-}
if [ -z "$version" ]; then
  echo "usage: install.sh v1.2.3" >&2
  exit 2
fi
case "$version" in
  v[0-9]* ) ;;
  * ) echo "release must look like v1.2.3" >&2; exit 2 ;;
esac
case "$version" in
  *[!A-Za-z0-9._-]* ) echo "release contains invalid characters" >&2; exit 2 ;;
esac

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "unsupported operating system" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "unsupported CPU architecture" >&2; exit 1 ;;
esac

archive="barq-${version}-${os}-${arch}.tar.gz"
base=${BARQ_RELEASE_BASE_URL:-"https://github.com/BarqDB/barq-control-plane/releases/download/${version}"}
install_dir=${BARQ_INSTALL_DIR:-"$HOME/.local/bin"}
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT HUP INT TERM

curl --fail --silent --show-error --location --output "$temporary/$archive" "$base/$archive"
curl --fail --silent --show-error --location --output "$temporary/$archive.sha256" "$base/$archive.sha256"
expected=$(awk 'NR == 1 {print $1}' "$temporary/$archive.sha256")
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$temporary/$archive" | awk '{print $1}')
else
  actual=$(shasum -a 256 "$temporary/$archive" | awk '{print $1}')
fi
if [ -z "$expected" ] || [ "$actual" != "$expected" ]; then
  echo "release checksum does not match" >&2
  exit 1
fi

mkdir -p "$temporary/package" "$install_dir"
tar -xzf "$temporary/$archive" -C "$temporary/package"
for program in barqctl restic cosign; do
  test -f "$temporary/package/$program"
  install -m 0755 "$temporary/package/$program" "$install_dir/$program"
done

echo "Barq $version installed in $install_dir"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "Add $install_dir to PATH, then run: barqctl init --domain db.example.com" ;;
esac
