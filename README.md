# shttpmocksrv

`shttpmocksrv` is a generic HTTP and HTTPS mock server for macOS. You decide what it answers: the JSON response for each URL is configured in a `routes.json` file that you write and edit. Nothing is built in. It is a single Go binary with no runtime dependency, started by a launchd agent.

## Install

On any Mac (Apple silicon or Intel), from the latest GitHub release:

```sh
curl -fsSL https://github.com/mishka81/simple-mock-http-server/releases/latest/download/install.sh | bash
```

`install.sh` downloads the binary for your CPU, verifies it against `SHA256SUMS`, creates `mock.env`, issues the TLS certificate with [mkcert](https://github.com/FiloSottile/mkcert) (installed with Homebrew if missing), then registers a launchd agent that starts at login. Ports below 1024 can be bound by a normal user on macOS, so no root is needed.

No response is installed. Write `~/.local/share/shttpmocksrv/routes.json` (see below), or pass your own file at install time with `--routes PATH`. Until it exists, every request answers `500`.

Options (pass after `bash -s --`): `--version TAG`, `--dir DIR`, `--http-port N`, `--https-port N`, `--hostnames "..."`, `--routes PATH`, `--dev` (build from the local source instead of downloading, needs Go), `--trust` (runs `mkcert -install`, asks for your password), `--hosts` (maps the `--hostnames` in `/etc/hosts`, asks for your password), `--no-service`, `--binary PATH` (install a local binary). See `install.sh --help`.

The installer is idempotent: running it again changes only what differs. The binary is replaced only if it differs from the release (or from the local build with `--dev`), the certificate is reissued only when the names change, the CA is missing or it expires within 30 days, the `/etc/hosts` block is rewritten only if it differs, and the launchd agent is restarted only when the binary, `mock.env` or the plist changed. An existing `routes.json` is never touched. Remove with `install.sh --uninstall` (add `--purge` to delete config and certs). The mkcert CA is shared with your other projects and is never removed (`mkcert -uninstall` does that).

Default location: `~/.local/share/shttpmocksrv` (`mock.env`, `routes.json`, `logs/shttpmocksrv.log`).

## Configuration

- `mock.env`: `HTTP_PORT` (default 80), `HTTPS_PORT` (default 443), `SERVER_NAMES` (hostnames and IPs in the certificate). After a change: `launchctl kickstart -k gui/$(id -u)/local.shttpmocksrv` (re-run the installer with `--hostnames` if `SERVER_NAMES` changed, to reissue the certificate).
- `routes.json`: the response for each URL. It is re-read on every request, so edits apply immediately.

### Routes

```json
{
  "routes": [
    {
      "method": "POST",
      "path": "/api/items",
      "status": 201,
      "body": { "id": 1 }
    },
    {
      "path": "/api/items/*",
      "file": "payloads/item.json"
    },
    {
      "path": "/*",
      "status": 404,
      "body": { "message": "not mocked" }
    }
  ]
}
```

- Routes are tried in order and the first match wins.
- `path`: exact (query string ignored), or a prefix when it ends with `*` (`/api/*`, `/*` for everything).
- `method`: optional, any method when omitted.
- `status`: optional, 200 by default. With neither `body` nor `file`, the response has an empty body (e.g. `204`).
- `body`: the JSON to return, inline. `file`: a file to return as is, relative to the install directory. Use one or the other.
- No route matching answers `404`, and a configuration error (missing or invalid `routes.json`, missing file) answers `500`, both as plain text with the reason.

### Scenarios

A route can define named variants of its response, e.g. to test an error on the same URL:

```json
{
  "path": "/api/items",
  "body": { "items": [] },
  "scenarios": {
    "full": { "body": { "items": [1, 2, 3] } },
    "down": { "status": 503, "body": { "message": "unavailable" } }
  }
}
```

Select one per request with a header or a query parameter (the header wins); an unknown name answers `500`:

```sh
curl -H 'X-Mock-Scenario: down' http://localhost/api/items
curl 'http://localhost/api/items?mock_scenario=full'
```

## Mocking a real hostname

```sh
install.sh --hostnames "localhost 127.0.0.1 ::1 api.example.com" --hosts --trust
```

- `--hostnames` adds the name to the certificate.
- `--hosts` maps every hostname of that list (except `localhost` and IPs) to `127.0.0.1` and `::1` in `/etc/hosts`. The lines go in a block marked `# BEGIN shttpmocksrv (...)`, which a later run replaces and `--uninstall` removes, so the real service is reachable again. A name you already mapped yourself is left alone.
- `--trust` makes the mkcert CA trusted system-wide, so browsers and `curl` accept the certificate. Firefox keeps its own store, filled through `certutil` (Homebrew's `nss`): the installer requires it and installs it if missing. Restart Firefox afterwards. A JVM does not use the macOS keychain: add `$(mkcert -CAROOT)/rootCA.pem` to its truststore.

## Logs

`logs/shttpmocksrv.log` shows each request (scheme, method, URI, scenario, status, form body), useful to check what a client sends.

## Development and release

From a clone of the repository, install your working copy instead of the release:

```sh
./install.sh --dev
```

```sh
go build -o shttpmocksrv .      # local binary; flags: -dir, -http-port, -https-port
go test ./...                  # handler tests
./test-install.sh              # end-to-end test of install.sh against a locally built release
./build-release.sh             # go vet, go test, then dist/: shttpmocksrv-darwin-{arm64,amd64}, install.sh, SHA256SUMS
gh release create v1.0.0 dist/* --repo mishka81/simple-mock-http-server --title v1.0.0
```

All of `dist/*` must be attached to the release: the installer downloads the binary and `SHA256SUMS` from it.
