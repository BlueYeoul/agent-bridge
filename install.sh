#!/usr/bin/env sh
set -eu

repo="${AGENT_BRIDGE_REPO:-BlueYeoul/agent-bridge}"
version="${AGENT_BRIDGE_VERSION:-latest}"
install_dir="${AGENT_BRIDGE_INSTALL_DIR:-$HOME/.local/bin}"
binary_name="agent-bridge"

need() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "agent-bridge installer: missing required command: $1" >&2
    exit 1
  fi
}

need uname
need mkdir
need tar

if command -v curl >/dev/null 2>&1; then
  download() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
  download() { wget -q "$1" -O "$2"; }
else
  echo "agent-bridge installer: curl or wget is required" >&2
  exit 1
fi

os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$os" in
  darwin) os="darwin" ;;
  linux) os="linux" ;;
  msys*|mingw*|cygwin*) os="windows" ;;
  *) echo "agent-bridge installer: unsupported OS: $os" >&2; exit 1 ;;
esac

arch="$(uname -m | tr '[:upper:]' '[:lower:]')"
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  arm64|aarch64) arch="arm64" ;;
  *) echo "agent-bridge installer: unsupported architecture: $arch" >&2; exit 1 ;;
esac

tmp_dir="$(mktemp -d 2>/dev/null || mktemp -d -t agent-bridge)"
cleanup() { rm -rf "$tmp_dir"; }
trap cleanup EXIT INT TERM

if [ "$os" = "windows" ]; then
  archive="agent-bridge_${os}_${arch}.zip"
  echo "agent-bridge installer: Windows users should prefer install.ps1" >&2
  exit 1
else
  archive="agent-bridge_${os}_${arch}.tar.gz"
fi

if [ "$version" = "latest" ]; then
  url="https://github.com/$repo/releases/latest/download/$archive"
else
  url="https://github.com/$repo/releases/download/$version/$archive"
fi

echo "agent-bridge installer: downloading $url"
download "$url" "$tmp_dir/$archive"

mkdir -p "$install_dir"
tar -xzf "$tmp_dir/$archive" -C "$tmp_dir"
install -m 0755 "$tmp_dir/$binary_name" "$install_dir/$binary_name"

echo "agent-bridge installer: installed $install_dir/$binary_name"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *)
    echo "agent-bridge installer: add this to your shell profile if needed:"
    echo "  export PATH=\"$install_dir:\$PATH\""
    ;;
esac

"$install_dir/$binary_name" --help >/dev/null
echo "agent-bridge installer: ready"
