#!/usr/bin/env sh
# Bootstrapper and version-synced launcher for the memorize MCP & Hook binary.
# All diagnostic output is sent strictly to stderr (>&2) to preserve MCP stdio and Hook JSON stdout.
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
PLUGIN_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
export MEMORY_PLUGIN_DIR="${MEMORY_PLUGIN_DIR:-$PLUGIN_DIR}"
export MEMORY_LAUNCHER_START_SEC="${MEMORY_LAUNCHER_START_SEC:-$(date +%s 2>/dev/null || echo '')}"

DATA_DIR="${MEMORY_DATA_DIR:-$HOME/.memorize}"
BIN_DIR="$DATA_DIR/bin"
export MEMORY_DB_PATH="${MEMORY_DB_PATH:-$DATA_DIR/memories.db}"
export MEMORY_MODELS_DIR="${MEMORY_MODELS_DIR:-$DATA_DIR/models}"

mkdir -p "$BIN_DIR" "$MEMORY_MODELS_DIR" 2>/dev/null || true
chmod 700 "$DATA_DIR" "$MEMORY_MODELS_DIR" 2>/dev/null || true

# 1. If a self-installed binary exists directly in $PLUGIN_DIR/bin/memorize and no remote override is forced,
# check manifest version first so plugin updates seamlessly upgrade the cached binary.
read_json_string() {
  key="$1"
  file="$2"
  if [ ! -f "$file" ]; then
    return 0
  fi
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$file" | head -n 1
}

VERSION="${MEMORIZE_VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION="$(read_json_string "version" "$PLUGIN_DIR/plugin.json")"
fi
if [ -z "$VERSION" ]; then
  VERSION="$(read_json_string "version" "$PLUGIN_DIR/.claude-plugin/plugin.json")"
fi
VERSION="${VERSION#v}"
if [ -z "$VERSION" ]; then
  VERSION="1.0.0"
fi
case "$VERSION" in
  ''|*[!0-9A-Za-z._-]*|.*|*..*)
    echo "[memorize] invalid plugin version: $VERSION" >&2
    exit 1
    ;;
esac

CACHED_BIN="$BIN_DIR/memorize-${VERSION}"
ACTIVE_LINK="$BIN_DIR/memorize"

install_binary_atomically() {
  src_bin="$1"
  tmp_dst="$BIN_DIR/.memorize-${VERSION}.tmp.$$"
  cp "$src_bin" "$tmp_dst" 2>/dev/null || return 1
  chmod 0755 "$tmp_dst"
  if [ "$(uname -s 2>/dev/null)" = "Darwin" ]; then
    command -v xattr >/dev/null 2>&1 && xattr -dr com.apple.quarantine "$tmp_dst" 2>/dev/null || true
    command -v codesign >/dev/null 2>&1 && codesign -s - -f "$tmp_dst" >/dev/null 2>&1 || true
  fi
  rm -f "$CACHED_BIN" 2>/dev/null || true
  mv -f "$tmp_dst" "$CACHED_BIN"
  ln -sf "$CACHED_BIN" "$ACTIVE_LINK" 2>/dev/null || true

  # Prune outdated versioned binaries while preserving the newly installed version.
  for old_bin in "$BIN_DIR"/memorize-*; do
    if [ -f "$old_bin" ] && [ "$old_bin" != "$CACHED_BIN" ]; then
      rm -f "$old_bin" 2>/dev/null || true
    fi
  done
}

# Auto-sync local development build only when invoked from within this repository's plugin tree.
dev_root="$(CDPATH= cd -- "$PLUGIN_DIR/../.." 2>/dev/null && pwd || true)"
if [ -n "$dev_root" ] && [ -f "$dev_root/go.mod" ] && [ -d "$dev_root/cmd/memorize" ] && [ -x "$dev_root/bin/memorize" ]; then
  if [ ! -x "$CACHED_BIN" ] || [ "$dev_root/bin/memorize" -nt "$CACHED_BIN" ]; then
    if ! install_binary_atomically "$dev_root/bin/memorize"; then
      exec "$dev_root/bin/memorize" "$@"
    fi
  fi
fi

# Fast path: target version binary is already cached and executable.
if [ -x "$CACHED_BIN" ]; then
  exec "$CACHED_BIN" "$@"
fi

# Acquire a POSIX directory lock so concurrent MCP + Hook startups do not race during download/build.
LOCK_DIR="$BIN_DIR/.bootstrap.lock"
LOCK_PID_FILE="$LOCK_DIR/pid"
LOCK_ACQUIRED=0
cleanup_lock() {
  if [ "$LOCK_ACQUIRED" -eq 1 ]; then
    rm -rf "$LOCK_DIR" 2>/dev/null || true
  fi
}
trap cleanup_lock EXIT INT TERM

WAIT_COUNT=0
while ! mkdir "$LOCK_DIR" 2>/dev/null; do
  if [ -x "$CACHED_BIN" ]; then
    exec "$CACHED_BIN" "$@"
  fi
  WAIT_COUNT=$((WAIT_COUNT + 1))
  lock_pid="$(cat "$LOCK_PID_FILE" 2>/dev/null || true)"
  case "$lock_pid" in
    ''|*[!0-9]*)
      if [ "$WAIT_COUNT" -ge 90 ]; then
        rm -rf "$LOCK_DIR" 2>/dev/null || true
        WAIT_COUNT=0
      fi
      ;;
    *)
      if [ "$WAIT_COUNT" -ge 5 ] && ! kill -0 "$lock_pid" 2>/dev/null; then
        rm -rf "$LOCK_DIR" 2>/dev/null || true
        WAIT_COUNT=0
      elif [ "$WAIT_COUNT" -ge 180 ]; then
        rm -rf "$LOCK_DIR" 2>/dev/null || true
        WAIT_COUNT=0
      fi
      ;;
  esac
  sleep 1
done
LOCK_ACQUIRED=1
printf '%s\n' "$$" > "$LOCK_PID_FILE" 2>/dev/null || true

# Re-check after acquiring lock in case another process just finished installing it.
if [ -x "$CACHED_BIN" ]; then
  cleanup_lock
  LOCK_ACQUIRED=0
  exec "$CACHED_BIN" "$@"
fi

detect_platform() {
  os_raw="$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]')"
  arch_raw="$(uname -m 2>/dev/null | tr '[:upper:]' '[:lower:]')"

  case "$os_raw" in
    darwin*) GOOS="darwin" ;;
    linux*)  GOOS="linux" ;;
    *)
      echo "[memorize] unsupported operating system: $os_raw" >&2
      return 1
      ;;
  esac

  case "$arch_raw" in
    arm64|aarch64) GOARCH="arm64" ;;
    x86_64|amd64)  GOARCH="amd64" ;;
    *)
      echo "[memorize] unsupported architecture: $arch_raw" >&2
      return 1
      ;;
  esac
  return 0
}

is_safe_release_url() {
  case "$1" in
    https://?*)
      return 0
      ;;
    http://127.0.0.1|http://127.0.0.1/*|http://127.0.0.1:*|http://127.0.0.1:*/*|http://localhost|http://localhost/*|http://localhost:*|http://localhost:*/*|http://\[::1\]|http://\[::1\]/*|http://\[::1\]:*|http://\[::1\]:*/*)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

resolve_release_base_url() {
  if [ -n "${MEMORIZE_RELEASE_BASE_URL:-}" ]; then
    # Substitute {VERSION} / ${VERSION} literally via awk (no sed delimiter or regex injection).
    base="$(awk -v raw="$MEMORIZE_RELEASE_BASE_URL" -v ver="$VERSION" '
      function replace_lit(s, pat, rep,    out, pos, plen) {
        out = ""
        plen = length(pat)
        while ((pos = index(s, pat)) > 0) {
          out = out substr(s, 1, pos - 1) rep
          s = substr(s, pos + plen)
        }
        return out s
      }
      BEGIN {
        raw = replace_lit(raw, "{VERSION}", ver)
        raw = replace_lit(raw, "${VERSION}", ver)
        printf "%s", raw
      }
    ')"
    is_safe_release_url "$base" || return 1
    printf "%s" "$base"
    return 0
  fi

  repo_url="$(read_json_string "repository" "$PLUGIN_DIR/plugin.json")"
  if [ -z "$repo_url" ]; then
    repo_url="$(read_json_string "repository" "$PLUGIN_DIR/.claude-plugin/plugin.json")"
  fi
  if [ -z "$repo_url" ] && command -v git >/dev/null 2>&1; then
    repo_url="$(git -C "$PLUGIN_DIR" remote get-url origin 2>/dev/null || true)"
  fi

  # Normalize SSH git@host:group/repo.git -> https://host/group/repo
  case "$repo_url" in
    git@*:* )
      host_part="${repo_url#git@}"
      host_part="${host_part%%:*}"
      path_part="${repo_url#*:}"
      repo_url="https://${host_part}/${path_part}"
      ;;
  esac
  repo_url="${repo_url%.git}"
  repo_url="${repo_url%/}"

  is_safe_release_url "$repo_url" || return 1

  case "$repo_url" in
    *github.com/*)
      if [ "$VERSION" = "latest" ]; then
        printf "%s/releases/latest/download" "$repo_url"
      else
        printf "%s/releases/download/v%s" "$repo_url" "$VERSION"
      fi
      ;;
    http://*/*|https://*/*)
      # GitLab release permanent asset download path
      if [ "$VERSION" = "latest" ]; then
        printf "%s/-/releases/permalink/latest/downloads" "$repo_url"
      else
        printf "%s/-/releases/v%s/downloads" "$repo_url" "$VERSION"
      fi
      ;;
    *)
      return 1
      ;;
  esac
  return 0
}

verify_checksum() {
  sums_file="$1"
  archive_name="$2"
  archive_path="$3"

  expected="$(awk -v target="$archive_name" 'NF >= 2 && ($2 == target || $2 == "./" target || $2 == "*" target) { print tolower($1); exit }' "$sums_file" 2>/dev/null || true)"
  case "$expected" in
    ''|*[!0-9a-f]*)
      echo "[memorize] valid SHA256 entry for $archive_name not found in SHA256SUMS" >&2
      return 1
      ;;
  esac
  if [ "${#expected}" -ne 64 ]; then
    echo "[memorize] malformed SHA256 checksum length for $archive_name" >&2
    return 1
  fi

  actual=""
  if command -v shasum >/dev/null 2>&1; then
    actual="$(shasum -a 256 "$archive_path" | awk '{print tolower($1)}')"
  elif command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$archive_path" | awk '{print tolower($1)}')"
  elif command -v openssl >/dev/null 2>&1; then
    actual="$(openssl dgst -sha256 "$archive_path" | awk '{print tolower($NF)}')"
  else
    echo "[memorize] no SHA256 verification tool (shasum/sha256sum/openssl) available" >&2
    return 1
  fi

  if [ "$expected" != "$actual" ]; then
    echo "[memorize] SHA256 mismatch for $archive_name (expected $expected, got $actual)" >&2
    return 1
  fi
  return 0
}

download_release_binary() {
  detect_platform || return 1
  base_url="$(resolve_release_base_url)" || return 1

  archive_name="memorize-${GOOS}-${GOARCH}.tar.gz"
  archive_url="${base_url%/}/${archive_name}"
  sums_url="${base_url%/}/SHA256SUMS"

  tmp_dir="$BIN_DIR/.dl-${VERSION}.$$"
  mkdir -p "$tmp_dir"

  echo "[memorize] downloading memorize v${VERSION} (${GOOS}/${GOARCH}) from ${base_url}..." >&2

  curl_fetch() {
    url="$1"
    out="$2"
    is_safe_release_url "$url" || return 1
    set -- -fsSL --connect-timeout 5 --max-time 30
    case "$url" in
      https://*) set -- "$@" --proto =https --proto-redir =https ;;
    esac
    if [ -n "${MEMORIZE_GITLAB_TOKEN:-${GITLAB_TOKEN:-}}" ]; then
      curl "$@" -H "PRIVATE-TOKEN: ${MEMORIZE_GITLAB_TOKEN:-${GITLAB_TOKEN}}" "$url" -o "$out"
    elif [ -n "${GITHUB_TOKEN:-}" ]; then
      curl "$@" -H "Authorization: Bearer ${GITHUB_TOKEN}" "$url" -o "$out"
    else
      curl "$@" "$url" -o "$out"
    fi
  }

  if ! command -v curl >/dev/null 2>&1; then
    rm -rf "$tmp_dir"
    return 1
  fi

  if ! curl_fetch "$archive_url" "$tmp_dir/$archive_name"; then
    rm -rf "$tmp_dir"
    return 1
  fi

  if ! curl_fetch "$sums_url" "$tmp_dir/SHA256SUMS" 2>/dev/null; then
    echo "[memorize] error: failed to fetch SHA256SUMS for v${VERSION}; refusing unverified archive" >&2
    rm -rf "$tmp_dir"
    return 1
  fi

  if ! verify_checksum "$tmp_dir/SHA256SUMS" "$archive_name" "$tmp_dir/$archive_name"; then
    rm -rf "$tmp_dir"
    return 1
  fi

  if ! tar -xzf "$tmp_dir/$archive_name" -C "$tmp_dir" memorize 2>/dev/null; then
    rm -rf "$tmp_dir"
    return 1
  fi

  if ! install_binary_atomically "$tmp_dir/memorize"; then
    rm -rf "$tmp_dir"
    return 1
  fi
  rm -rf "$tmp_dir"
  echo "[memorize] installed memorize v${VERSION} to ${CACHED_BIN}" >&2
  return 0
}

try_local_fallbacks() {
  # Fallback 1: Binary bundled directly inside the plugin directory (e.g. via `memorize -install`).
  if [ -x "$PLUGIN_DIR/bin/memorize" ]; then
    if install_binary_atomically "$PLUGIN_DIR/bin/memorize"; then
      return 0
    fi
  fi

  # Fallback 2: Repository root prebuilt binary or explicit Go dev build from this plugin's own checkout.
  REPO_ROOT="$(CDPATH= cd -- "$PLUGIN_DIR/../.." 2>/dev/null && pwd || true)"
  if [ -n "$REPO_ROOT" ] && [ -f "$REPO_ROOT/go.mod" ] && [ -d "$REPO_ROOT/cmd/memorize" ]; then
    if [ -x "$REPO_ROOT/bin/memorize" ]; then
      if install_binary_atomically "$REPO_ROOT/bin/memorize"; then
        return 0
      fi
    fi
    if [ "${MEMORIZE_DEV_BUILD:-0}" = "1" ] && command -v go >/dev/null 2>&1; then
      echo "[memorize] building memorize v${VERSION} from local Go source..." >&2
      tmp_build="$BIN_DIR/.memorize-build.tmp.$$"
      if (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o "$tmp_build" ./cmd/memorize >&2); then
        if install_binary_atomically "$tmp_build"; then
          rm -f "$tmp_build"
          return 0
        fi
      fi
      rm -f "$tmp_build"
    fi
  fi

  # Fallback 3: Previously cached binary if network is temporarily unreachable during a version bump.
  if [ -x "$ACTIVE_LINK" ]; then
    echo "[memorize] warning: could not fetch v${VERSION}; falling back to cached binary" >&2
    return 0
  fi

  return 1
}

if ! download_release_binary; then
  if ! try_local_fallbacks; then
    echo "[memorize] error: failed to acquire memorize v${VERSION} binary (set repository in plugin.json or MEMORIZE_RELEASE_BASE_URL)" >&2
    exit 1
  fi
fi

cleanup_lock
LOCK_ACQUIRED=0

if [ -x "$CACHED_BIN" ]; then
  exec "$CACHED_BIN" "$@"
fi
exec "$ACTIVE_LINK" "$@"
