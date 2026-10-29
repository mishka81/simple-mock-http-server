#!/bin/bash
# End-to-end test of install.sh on macOS: install from a locally built release (file:// base URL, no service,
# unprivileged ports) with routes supplied by the test, serve over HTTP and HTTPS, check that user edits are
# preserved and checksums enforced, then uninstall.
# A dedicated name and --no-service keep it away from any real installation (agent, keychain).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

WORK="$(mktemp -d)"
DIR="$WORK/install"
# Never touch the real /etc/hosts from a test
export SHTTPMOCKSRV_HOSTS_FILE="$WORK/hosts"
PID=""
FAILURES=0
SVC_NAME="shttpmocksrv-test-svc"
cleanup() {
    [ -z "$PID" ] || kill "$PID" 2>/dev/null || true
    launchctl bootout "gui/$(id -u)/local.$SVC_NAME" 2>/dev/null || true
    rm -f "$HOME/Library/LaunchAgents/local.$SVC_NAME.plist"
    rm -rf "$WORK"
}
trap cleanup EXIT

check() {
    if [ "$2" = "$3" ]; then
        echo "ok   $1"
    else
        echo "FAIL $1: got '$2', want '$3'"
        FAILURES=$((FAILURES + 1))
    fi
}

# GIVEN mkcert (the installer uses its CA, which must never be modified) and a local release built by build-release.sh
command -v mkcert >/dev/null || { echo "mkcert is required"; exit 1; }
CA="$(mkcert -CAROOT)/rootCA.pem"
[ -f "$CA" ] || { echo "no mkcert CA yet, run: mkcert -install"; exit 1; }
CA_BEFORE="$(cksum <"$CA")"
DIST="$WORK/release" "$ROOT/build-release.sh" >"$WORK/build.log" 2>&1 || { cat "$WORK/build.log"; exit 1; }
RELEASE="file://$WORK/release"

# GIVEN the user's own routes
cat >"$WORK/my-routes.json" <<'JSON'
{"routes": [
  {"path": "/a", "body": {"url": "a"}, "scenarios": {"other": {"status": 202, "body": {"url": "a-other"}}}},
  {"path": "/b/*", "status": 201, "body": {"url": "b"}}
]}
JSON

install() {
    "$ROOT/install.sh" --base-url "$RELEASE" --name shttpmocksrv-test --dir "$DIR" \
        --http-port 18080 --https-port 18443 --hostnames "localhost 127.0.0.1 ::1 api.example.test" \
        --no-service "$@" >"$WORK/install.log" 2>&1
}

start_server() {
    "$DIR/shttpmocksrv" -dir "$DIR" >"$WORK/server.log" 2>&1 &
    PID=$!
    for _ in 1 2 3 4 5 6 7 8 9 10; do curl -s -o /dev/null http://localhost:18080/ && break; sleep 0.3; done
}

stop_server() {
    kill "$PID"
    wait "$PID" 2>/dev/null || true
    PID=""
}

# WHEN installing without any routes
install || { cat "$WORK/install.log"; exit 1; }

# THEN nothing is invented for the user
check "binary installed" "$([ -x "$DIR/shttpmocksrv" ] && echo yes)" "yes"
check "no routes.json created" "$([ -e "$DIR/routes.json" ] && echo present || echo absent)" "absent"
check "user is told to write routes.json" "$(grep -c 'no routes.json yet' "$WORK/install.log")" "1"
check "ports written to mock.env" "$(grep -E '^(HTTP|HTTPS)_PORT' "$DIR/mock.env" | tr '\n' ' ')" "HTTP_PORT=18080 HTTPS_PORT=18443 "
check "certificate SAN covers the custom hostname" "$(openssl x509 -in "$DIR/certs/mock.pem" -noout -text | grep -c 'DNS:api.example.test')" "1"
check "certificate verifies against the CA" "$(openssl verify -CAfile "$CA" "$DIR/certs/mock.pem" 2>&1 | tail -1 | sed 's/.*: //')" "OK"

# WHEN the server runs without routes
start_server

# THEN it answers with an error instead of inventing a response
check "no routes.json answers 500" "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/a)" "500"
stop_server

# WHEN installing with the user's routes
install --routes "$WORK/my-routes.json" || { cat "$WORK/install.log"; exit 1; }
start_server

# THEN each URL gets the user's response over HTTP and HTTPS
check "route /a" "$(curl -s http://localhost:18080/a)" '{"url": "a"}'
check "route /b/* status and body" "$(curl -s -w ' %{http_code}' http://localhost:18080/b/x/y)" '{"url": "b"} 201'
check "scenario header" "$(curl -s -w ' %{http_code}' -H 'X-Mock-Scenario: other' http://localhost:18080/a)" '{"url": "a-other"} 202'
check "unknown URL" "$(curl -s -o /dev/null -w '%{http_code}' http://localhost:18080/c)" "404"
check "HTTPS trusted via the mkcert CA" "$(curl -s --cacert "$CA" https://localhost:18443/a)" '{"url": "a"}'
check "HTTPS via hostname override" "$(curl -s --cacert "$CA" --resolve api.example.test:18443:127.0.0.1 https://api.example.test:18443/a)" '{"url": "a"}'
stop_server

# WHEN the installer runs again with the same arguments
fingerprint() { cat "$DIR/shttpmocksrv" "$DIR/mock.env" "$DIR/routes.json" "$DIR/certs/mock.pem" "$DIR/certs/mock-key.pem" | cksum; }
BEFORE="$(fingerprint)"
install --routes "$WORK/my-routes.json" || { cat "$WORK/install.log"; exit 1; }

# THEN nothing changes and it says so
check "idempotent: files unchanged" "$(fingerprint)" "$BEFORE"
check "idempotent: binary kept" "$(grep -c 'Binary already up to date' "$WORK/install.log")" "1"
check "idempotent: certificate kept" "$(grep -c 'certificate already up to date' "$WORK/install.log")" "1"

# WHEN the certificate names change
install --routes "$WORK/my-routes.json" --hostnames "localhost 127.0.0.1 ::1 other.example.test" || { cat "$WORK/install.log"; exit 1; }

# THEN the certificate is reissued for the new names
check "certificate reissued for new names" "$(openssl x509 -in "$DIR/certs/mock.pem" -noout -text | grep -c 'DNS:other.example.test')" "1"
install --routes "$WORK/my-routes.json" || { cat "$WORK/install.log"; exit 1; }

# WHEN the user edits routes.json and the installer runs again with other routes
echo '{"routes": [{"path": "/edited", "body": {"edited": true}}]}' >"$DIR/routes.json"
echo '{"routes": []}' >"$WORK/other-routes.json"
install --routes "$WORK/other-routes.json" || { cat "$WORK/install.log"; exit 1; }

# THEN the edits are preserved and the shared CA is untouched
check "edited routes.json preserved" "$(grep -c '"edited"' "$DIR/routes.json")" "1"
check "mkcert CA untouched" "$(cksum <"$CA")" "$CA_BEFORE"

# WHEN installing with --hosts on a hosts file with user lines (the last one without trailing newline)
printf '10.0.0.5 keep.example.test\n127.0.0.1 manual.example.test' >"$SHTTPMOCKSRV_HOSTS_FILE"
HOSTNAMES="localhost 127.0.0.1 ::1 api.example.test manual.example.test"
install --hosts --hostnames "$HOSTNAMES" || { cat "$WORK/install.log"; exit 1; }
install --hosts --hostnames "$HOSTNAMES" || { cat "$WORK/install.log"; exit 1; }

# THEN only the missing name is mapped, once, and the user's lines are kept
check "hosts: IPv4 entry added" "$(grep -c '^127.0.0.1 api.example.test$' "$SHTTPMOCKSRV_HOSTS_FILE")" "1"
check "hosts: IPv6 entry added" "$(grep -c '^::1 api.example.test$' "$SHTTPMOCKSRV_HOSTS_FILE")" "1"
check "hosts: name mapped by the user left alone" "$(grep -c 'manual.example.test' "$SHTTPMOCKSRV_HOSTS_FILE")" "1"
check "hosts: localhost not added" "$(grep -c 'localhost' "$SHTTPMOCKSRV_HOSTS_FILE")" "0"
check "hosts: user lines preserved" "$(grep -c '^10.0.0.5 keep.example.test$' "$SHTTPMOCKSRV_HOSTS_FILE")" "1"
check "hosts: block not duplicated by a second run" "$(grep -c '^# BEGIN shttpmocksrv' "$SHTTPMOCKSRV_HOSTS_FILE")" "1"

# WHEN installing the launchd agent twice with the same arguments, then with another port
SVC_DIR="$WORK/svc"
svc_install() {
    "$ROOT/install.sh" --base-url "$RELEASE" --name "$SVC_NAME" --dir "$SVC_DIR" --http-port "$1" --https-port 18444 \
        --routes "$WORK/my-routes.json" >"$WORK/svc.log" 2>&1
}
svc_pid() { launchctl print "gui/$(id -u)/local.$SVC_NAME" | awk '/^[[:space:]]*pid = /{print $3}'; }

svc_install 18081 || { cat "$WORK/svc.log"; exit 1; }
PID1="$(svc_pid)"
check "service: running and answering" "$(curl -s http://localhost:18081/a)" '{"url": "a"}'
svc_install 18081 || { cat "$WORK/svc.log"; exit 1; }
check "service: not restarted when nothing changed" "$(svc_pid)" "$PID1"
check "service: says it kept the agent" "$(grep -c 'not restarting it' "$WORK/svc.log")" "1"
svc_install 18082 || { cat "$WORK/svc.log"; exit 1; }
check "service: restarted when the port changed" "$([ "$(svc_pid)" != "$PID1" ] && echo restarted)" "restarted"
check "service: answers on the new port" "$(curl -s http://localhost:18082/a)" '{"url": "a"}'
"$ROOT/install.sh" --name "$SVC_NAME" --dir "$SVC_DIR" --uninstall --purge >/dev/null 2>&1
check "service: agent removed on uninstall" "$(launchctl print "gui/$(id -u)/local.$SVC_NAME" >/dev/null 2>&1 && echo loaded || echo gone)" "gone"

# WHEN installing in dev mode (build from the local source)
dev_install() { "$ROOT/install.sh" --dev --name shttpmocksrv-test --dir "$WORK/dev" --no-service "$@" >"$WORK/dev.log" 2>&1; }
dev_install || { cat "$WORK/dev.log"; exit 1; }

# THEN the binary is built locally, not downloaded, and a second run keeps it
check "dev: binary built from source" "$(file "$WORK/dev/shttpmocksrv" | grep -c 'Mach-O')" "1"
check "dev: nothing downloaded" "$(grep -c 'Downloading' "$WORK/dev.log")" "0"
dev_install || { cat "$WORK/dev.log"; exit 1; }
check "dev: idempotent rebuild keeps the binary" "$(grep -c 'Binary already up to date' "$WORK/dev.log")" "1"
check "dev: refuses --binary" "$("$ROOT/install.sh" --dev --binary /bin/ls --dir "$WORK/x" --no-service 2>&1 | grep -c 'mutually exclusive')" "1"
mkdir "$WORK/lonely" && cp "$ROOT/install.sh" "$WORK/lonely/"
check "dev: needs the source next to the script" "$("$WORK/lonely/install.sh" --dev --dir "$WORK/x" --no-service 2>&1 | grep -c 'needs the source')" "1"

# WHEN a downloaded binary does not match SHA256SUMS
echo "tampered" >>"$WORK/release/shttpmocksrv-darwin-$(uname -m | sed 's/x86_64/amd64/')"
RESULT="$("$ROOT/install.sh" --base-url "$RELEASE" --name shttpmocksrv-test --dir "$WORK/other" --no-service 2>&1 || true)"

# THEN the install is refused
check "bad checksum rejected" "$(echo "$RESULT" | grep -c 'checksum mismatch')" "1"
check "nothing installed after bad checksum" "$([ -e "$WORK/other/shttpmocksrv" ] && echo present || echo absent)" "absent"

# WHEN uninstalling with --purge
"$ROOT/install.sh" --name shttpmocksrv-test --dir "$DIR" --uninstall --purge --no-service >/dev/null 2>&1
check "install dir purged" "$([ -d "$DIR" ] && echo present || echo gone)" "gone"
check "hosts: block removed on uninstall" "$(grep -c 'shttpmocksrv' "$SHTTPMOCKSRV_HOSTS_FILE")" "0"
check "hosts: user lines kept on uninstall" "$(grep -c 'keep.example.test' "$SHTTPMOCKSRV_HOSTS_FILE")" "1"

if [ "$FAILURES" -gt 0 ]; then
    echo "$FAILURES check(s) failed"
    exit 1
fi
echo "all checks passed"
