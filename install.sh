#!/usr/bin/env bash
# Installs the mock HTTP server on macOS and starts it at login (launchd agent).
#
#   curl -fsSL https://github.com/mishka81/simple-mock-http-server/releases/latest/download/install.sh | bash
#
# Downloads the binary from the GitHub release assets (verified against SHA256SUMS, produced by build-release.sh).
# The config and the TLS certificate are created locally. No response is installed: you write routes.json.
set -euo pipefail

REPO="${SHTTPMOCKSRV_REPO:-mishka81/simple-mock-http-server}"
VERSION="latest"
BASE_URL=""
NAME="shttpmocksrv"
DIR=""
BINARY=""
ROUTES=""
HTTP_PORT=""
HTTPS_PORT=""
HOSTNAMES=""
TRUST=0
HOSTS=0
DEV=0
CHANGED=0
NO_SERVICE=0
UNINSTALL=0
PURGE=0

usage() {
    cat <<'EOF'
Usage: install.sh [options]

Install
  --version TAG      release to install (default: latest)
  --repo OWNER/NAME  GitHub repository (default: mishka81/simple-mock-http-server, or env SHTTPMOCKSRV_REPO)
  --binary PATH      install this local binary instead of downloading
  --dev              build the binary from the source next to this script (needs Go) instead of downloading
  --routes PATH      your routes.json, installed only when none exists yet (otherwise write it yourself)
  --dir DIR          install directory (default: ~/.local/share/<name>)
  --name NAME        service name (default: shttpmocksrv)
  --http-port N      HTTP port (default: 80, kept from an existing mock.env when omitted)
  --https-port N     HTTPS port (default: 443)
  --hostnames "..."  names/IPs in the TLS certificate, space separated
                     (default: localhost 127.0.0.1 ::1)
  --trust            run 'mkcert -install' to trust the mkcert CA system-wide, Firefox included (asks for your password;
                     installs certutil from Homebrew's nss if missing)
  --hosts            map each hostname of --hostnames (except localhost and IPs) to 127.0.0.1 and ::1 in
                     /etc/hosts (asks for your password), in a block that --uninstall removes
  --no-service       install files only, do not register or start the launchd agent
  --base-url URL     download from URL/<asset> instead of GitHub (testing)

Remove
  --uninstall        stop and unregister the agent, remove the binary (keeps config and certs)
  --purge            with --uninstall: also delete the install directory (the shared mkcert CA stays)
                     (--no-service with --uninstall leaves the launchd agent untouched)

  -h, --help         show this help

Running it again is safe: unchanged files, certificate, /etc/hosts block and launchd agent are left as they
are (the agent is only restarted when the binary, the configuration or its plist changed).
EOF
}

log() { printf '==> %s\n' "$*"; }
warn() { printf 'warning: %s\n' "$*" >&2; }
die() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

need_value() { [ "$2" -ge 2 ] || die "$1 needs a value"; }

while [ $# -gt 0 ]; do
    case "$1" in
    --version) need_value "$1" $#; VERSION="$2"; shift 2 ;;
    --repo) need_value "$1" $#; REPO="$2"; shift 2 ;;
    --base-url) need_value "$1" $#; BASE_URL="$2"; shift 2 ;;
    --binary) need_value "$1" $#; BINARY="$2"; shift 2 ;;
    --dev) DEV=1; shift ;;
    --routes) need_value "$1" $#; ROUTES="$2"; shift 2 ;;
    --dir) need_value "$1" $#; DIR="$2"; shift 2 ;;
    --name) need_value "$1" $#; NAME="$2"; shift 2 ;;
    --http-port) need_value "$1" $#; HTTP_PORT="$2"; shift 2 ;;
    --https-port) need_value "$1" $#; HTTPS_PORT="$2"; shift 2 ;;
    --hostnames) need_value "$1" $#; HOSTNAMES="$2"; shift 2 ;;
    --trust) TRUST=1; shift ;;
    --hosts) HOSTS=1; shift ;;
    --no-service) NO_SERVICE=1; shift ;;
    --uninstall) UNINSTALL=1; shift ;;
    --purge) PURGE=1; shift ;;
    -h | --help) usage; exit 0 ;;
    *) usage >&2; die "unknown option: $1" ;;
    esac
done

[ "$(uname -s)" = "Darwin" ] || die "macOS only"
[ "$DEV" -eq 0 ] || [ -z "$BINARY" ] || die "--dev and --binary are mutually exclusive"
[ -n "$DIR" ] || DIR="$HOME/.local/share/$NAME"
case "$NAME" in *[!A-Za-z0-9_.-]* | '') die "invalid --name '$NAME' (letters, digits, '_', '.', '-')" ;; esac
for port in "$HTTP_PORT" "$HTTPS_PORT"; do
    case "$port" in *[!0-9]*) die "invalid port '$port'" ;; esac
done

case "$(uname -m)" in
arm64) ARCH="arm64" ;;
x86_64) ARCH="amd64" ;;
*) die "unsupported CPU '$(uname -m)'" ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
LABEL="local.$NAME"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
GUI="gui/$(id -u)"

get_env() { sed -n "s/^$1=//p" "$DIR/mock.env" 2>/dev/null | tail -1 | tr -d "\"'"; }

set_env() {
    local before
    before="$(cksum <"$DIR/mock.env")"
    sed -i.bak "s|^$1=.*|$1=$2|" "$DIR/mock.env"
    rm -f "$DIR/mock.env.bak"
    [ "$before" = "$(cksum <"$DIR/mock.env")" ] || CHANGED=1
}

stop_service() {
    launchctl bootout "$GUI/$LABEL" 2>/dev/null || true
    rm -f "$PLIST"
}

HOSTS_FILE="${SHTTPMOCKSRV_HOSTS_FILE:-/etc/hosts}"
HOSTS_BEGIN="# BEGIN shttpmocksrv ($NAME)"
HOSTS_END="# END shttpmocksrv ($NAME)"

hosts_without_block() {
    awk -v b="$HOSTS_BEGIN" -v e="$HOSTS_END" '$0 == b {skip = 1; next} $0 == e {skip = 0; next} !skip' "$HOSTS_FILE"
}

write_hosts() {
    if [ -w "$HOSTS_FILE" ]; then
        cp "$1" "$HOSTS_FILE"
    else
        sudo cp "$1" "$HOSTS_FILE"
    fi
}

# Maps the hostnames of SERVER_NAMES to this machine, in a marked block so it can be replaced or removed cleanly.
# A name already mapped by a line of your own is left alone.
add_hosts() {
    local names n esc block="" tmp
    names="$(get_env SERVER_NAMES)"
    tmp="$(mktemp)"
    hosts_without_block >"$tmp"
    [ -z "$(tail -c1 "$tmp")" ] || echo >>"$tmp"

    for n in $names; do
        case "$n" in localhost | *:* | [0-9]*.[0-9]*.[0-9]*.[0-9]*) continue ;; esac
        esc="$(printf '%s' "$n" | sed 's/\./\\./g')"
        if grep -Eq "^[^#]*[[:space:]]$esc([[:space:]]|\$)" "$tmp"; then
            log "$n is already in $HOSTS_FILE, left as is"
            continue
        fi
        block="${block}127.0.0.1 $n"$'\n'"::1 $n"$'\n'
    done

    if [ -z "$block" ]; then
        log "No hostname to add to $HOSTS_FILE"
    else
        printf '%s\n%s%s\n' "$HOSTS_BEGIN" "$block" "$HOSTS_END" >>"$tmp"
        if cmp -s "$tmp" "$HOSTS_FILE"; then
            log "$HOSTS_FILE is already up to date"
        else
            log "Updating $HOSTS_FILE (may ask for your password)"
            write_hosts "$tmp"
            printf '%s' "$block" | sed 's/^/    /'
        fi
    fi
    rm -f "$tmp"
}

remove_hosts() {
    [ -f "$HOSTS_FILE" ] && grep -qxF "$HOSTS_BEGIN" "$HOSTS_FILE" || return 0
    local tmp
    tmp="$(mktemp)"
    hosts_without_block >"$tmp"
    log "Removing the shttpmocksrv block from $HOSTS_FILE (may ask for your password)"
    write_hosts "$tmp"
    rm -f "$tmp"
}

if [ "$UNINSTALL" -eq 1 ]; then
    if [ "$NO_SERVICE" -eq 0 ]; then
        log "Stopping agent '$LABEL'"
        stop_service
    fi
    remove_hosts
    rm -f "$DIR/shttpmocksrv"
    if [ "$PURGE" -eq 1 ]; then
        log "Removing '$DIR' (the mkcert CA is shared and left in place, 'mkcert -uninstall' removes it)"
        rm -rf "$DIR"
    else
        log "Kept config and certs in '$DIR' (use --purge to delete)"
    fi
    log "Uninstalled"
    exit 0
fi

RELEASE_TMP=""

release_url() {
    if [ -n "$BASE_URL" ]; then
        echo "$BASE_URL"
    elif [ "$VERSION" = "latest" ]; then
        echo "https://github.com/$REPO/releases/latest/download"
    else
        echo "https://github.com/$REPO/releases/download/$VERSION"
    fi
}

fetch_sums() {
    [ -f "$RELEASE_TMP/SHA256SUMS" ] && return 0
    local url
    url="$(release_url)"
    curl -fsSL -o "$RELEASE_TMP/SHA256SUMS" "$url/SHA256SUMS" || die "cannot download $url/SHA256SUMS (does the release exist?)"
}

expected_sum() {
    awk -v f="$1" '$2 == f || $2 == "*" f {print $1}' "$RELEASE_TMP/SHA256SUMS"
}

sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }

# Downloads a release asset to $2 after checking it against the release's SHA256SUMS.
download_asset() {
    local asset="$1" dest="$2" url expected actual
    url="$(release_url)"
    fetch_sums
    log "Downloading $asset from $url"
    curl -fsSL -o "$RELEASE_TMP/$asset" "$url/$asset" || die "cannot download $url/$asset"
    expected="$(expected_sum "$asset")"
    [ -n "$expected" ] || die "$asset is not listed in SHA256SUMS"
    actual="$(sha256_of "$RELEASE_TMP/$asset")"
    [ "$expected" = "$actual" ] || die "checksum mismatch for $asset (expected $expected, got $actual)"
    mv -f "$RELEASE_TMP/$asset" "$dest"
}

# --dev: builds the binary from the source next to this script, with the same flags as build-release.sh.
build_local() {
    [ -f "$SCRIPT_DIR/main.go" ] || die "--dev needs the source: run install.sh from a clone of the repository"
    PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"
    command -v go >/dev/null 2>&1 || die "--dev needs Go (brew install go)"
    log "Building from source in $SCRIPT_DIR"
    (cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$1" .) || die "go build failed"
}

# Leaves an identical installed binary alone (no download, no restart); sets CHANGED=1 when it replaces it.
install_binary() {
    mkdir -p "$DIR"
    local src="$RELEASE_TMP/shttpmocksrv" asset

    if [ -n "$BINARY" ]; then
        [ -f "$BINARY" ] || die "binary not found: $BINARY"
        cp "$BINARY" "$src"
    elif [ "$DEV" -eq 1 ]; then
        build_local "$src"
    else
        asset="shttpmocksrv-darwin-$ARCH"
        fetch_sums
        if [ -f "$DIR/shttpmocksrv" ] && [ "$(sha256_of "$DIR/shttpmocksrv")" = "$(expected_sum "$asset")" ]; then
            log "Binary already up to date"
            return 0
        fi
        download_asset "$asset" "$src"
    fi

    chmod 755 "$src"
    if cmp -s "$src" "$DIR/shttpmocksrv"; then
        log "Binary already up to date"
        return 0
    fi
    # Copy then rename: replacing a running executable in place can get it killed on macOS.
    cp "$src" "$DIR/shttpmocksrv.new"
    mv -f "$DIR/shttpmocksrv.new" "$DIR/shttpmocksrv"
    CHANGED=1
    log "Installed binary to $DIR/shttpmocksrv"
}

write_config() {
    mkdir -p "$DIR/certs" "$DIR/logs"

    if [ ! -f "$DIR/mock.env" ]; then
        cat >"$DIR/mock.env" <<'EOF'
# Mock server configuration (read by the shttpmocksrv binary).
# The responses are configured per URL in routes.json (re-read on every request, no restart needed).
# Changing ports requires a restart of the agent.

HTTP_PORT=80
HTTPS_PORT=443

# Hostnames and IPs covered by the TLS certificate (space separated).
# Re-run the installer after changing this.
SERVER_NAMES="localhost 127.0.0.1 ::1"
EOF
        CHANGED=1
    fi
    [ -z "$HTTP_PORT" ] || set_env HTTP_PORT "$HTTP_PORT"
    [ -z "$HTTPS_PORT" ] || set_env HTTPS_PORT "$HTTPS_PORT"
    [ -z "$HOSTNAMES" ] || set_env SERVER_NAMES "\"$HOSTNAMES\""
}

# The responses are yours to write: an existing routes.json is never touched, and none is created from scratch.
install_routes() {
    if [ -f "$DIR/routes.json" ]; then
        log "Kept your existing $DIR/routes.json"
    elif [ -n "$ROUTES" ]; then
        [ -f "$ROUTES" ] || die "routes file not found: $ROUTES"
        cp "$ROUTES" "$DIR/routes.json"
        log "Installed $DIR/routes.json from $ROUTES"
    else
        warn "no routes.json yet: create $DIR/routes.json (see the README), every request answers 500 until then"
    fi
}

ensure_mkcert() {
    command -v mkcert >/dev/null 2>&1 && return 0
    if command -v brew >/dev/null 2>&1; then
        log "Installing mkcert with Homebrew"
        brew install mkcert || die "brew install mkcert failed"
    else
        die "mkcert is required: install it from https://github.com/FiloSottile/mkcert (brew install mkcert)"
    fi
}

# True when the certificate exists, was issued for exactly these names by the current mkcert CA, and is valid for 30+ days.
cert_is_current() {
    local certs="$DIR/certs"
    [ -f "$certs/mock.pem" ] && [ -f "$certs/mock-key.pem" ] || return 1
    [ "$(cat "$certs/names" 2>/dev/null)" = "$1" ] || return 1
    openssl x509 -in "$certs/mock.pem" -noout -checkend $((30 * 24 * 3600)) >/dev/null 2>&1 || return 1
    openssl verify -CAfile "$(mkcert -CAROOT)/rootCA.pem" "$certs/mock.pem" >/dev/null 2>&1
}

# Server certificate for SERVER_NAMES, signed by the mkcert CA. Reissued only when needed; sets CHANGED=1 then.
generate_certs() {
    ensure_mkcert
    local names
    names="$(get_env SERVER_NAMES)"
    [ -n "$names" ] || names="localhost 127.0.0.1 ::1"

    if cert_is_current "$names"; then
        log "Server certificate already up to date for: $names"
        return 0
    fi
    log "Creating server certificate with mkcert for: $names"
    # shellcheck disable=SC2086 # SERVER_NAMES is a space separated list
    mkcert -cert-file "$DIR/certs/mock.pem" -key-file "$DIR/certs/mock-key.pem" $names >/dev/null 2>&1 ||
        die "mkcert failed to create the certificate (run it by hand to see why)"
    chmod 600 "$DIR/certs/mock-key.pem"
    printf '%s' "$names" >"$DIR/certs/names"
    CHANGED=1
}

# Firefox keeps its own certificate store, which mkcert only fills through certutil (Homebrew's nss).
ensure_certutil() {
    if command -v certutil >/dev/null 2>&1 || [ -x "$(brew --prefix nss 2>/dev/null)/bin/certutil" ]; then
        return 0
    fi
    command -v brew >/dev/null 2>&1 || die "certutil is required to trust the CA in Firefox: install Homebrew's nss (brew install nss)"
    log "Installing certutil (nss) with Homebrew, required to trust the CA in Firefox"
    brew install nss || die "brew install nss failed"
}

# Trusts the mkcert CA system-wide (keychain, Java, Firefox). Shared with all your mkcert certificates.
trust_ca() {
    ensure_certutil
    log "Running 'mkcert -install' (may ask for your password)"
    mkcert -install
}

install_service() {
    local new_plist="$RELEASE_TMP/agent.plist" loaded=0
    mkdir -p "$HOME/Library/LaunchAgents"
    cat >"$new_plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$DIR/shttpmocksrv</string>
        <string>-dir</string>
        <string>$DIR</string>
    </array>
    <key>WorkingDirectory</key>
    <string>$DIR</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>StandardOutPath</key>
    <string>$DIR/logs/shttpmocksrv.log</string>
    <key>StandardErrorPath</key>
    <string>$DIR/logs/shttpmocksrv.log</string>
</dict>
</plist>
EOF
    launchctl print "$GUI/$LABEL" >/dev/null 2>&1 && loaded=1
    if [ "$loaded" -eq 1 ] && [ "$CHANGED" -eq 0 ] && cmp -s "$new_plist" "$PLIST"; then
        log "launchd agent '$LABEL' is already loaded and up to date, not restarting it"
        return 0
    fi
    cp "$new_plist" "$PLIST"
    if [ "$loaded" -eq 1 ]; then
        launchctl bootout "$GUI/$LABEL" 2>/dev/null || true
        sleep 1
    fi
    launchctl bootstrap "$GUI" "$PLIST"
    log "launchd agent '$LABEL' loaded (starts at login)"
}

wait_for_server() {
    local i
    for i in 1 2 3 4 5 6 7 8 9 10; do
        if curl -fs -o /dev/null "http://localhost:$(get_env HTTP_PORT)/"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

RELEASE_TMP="$(mktemp -d)"
trap 'rm -rf "$RELEASE_TMP"' EXIT

install_binary
write_config
install_routes
generate_certs
[ "$TRUST" -eq 0 ] || trust_ca
[ "$HOSTS" -eq 0 ] || add_hosts

if [ "$NO_SERVICE" -eq 0 ]; then
    install_service
    if wait_for_server; then
        log "Server answers on http://localhost:$(get_env HTTP_PORT)/"
    else
        warn "server did not answer, check $DIR/logs/shttpmocksrv.log"
    fi
fi

cat <<EOF

Installed in $DIR
  config:     $DIR/mock.env
  routes:     $DIR/routes.json (your responses per URL, edit freely, no restart needed)
  logs:       $DIR/logs/shttpmocksrv.log
  CA:         $(mkcert -CAROOT)/rootCA.pem (mkcert)

HTTPS clients must trust the CA: rerun with --trust ('mkcert -install'), or pass it explicitly, e.g.
  curl --cacert "\$(mkcert -CAROOT)/rootCA.pem" https://localhost:$(get_env HTTPS_PORT)/
Java clients (JVM truststore):
  --truststore-paths="\$(mkcert -CAROOT)/rootCA.pem"

To mock a real hostname, add it to --hostnames and use --hosts to map it in /etc/hosts, e.g.:
  install.sh --hostnames "localhost 127.0.0.1 ::1 api.example.com" --hosts --trust
Uninstall:  install.sh --uninstall [--purge]   (name: $NAME)
EOF
