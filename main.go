// Mock HTTP server: answers every request with the JSON configured for its URL in routes.json.
//
// routes.json is re-read on every request. The first route matching the request wins. A route has an optional
// method, a path (exact, or a prefix when it ends with '*'), and a response: status (default 200) plus either an
// inline body or a file (relative to the project directory). A route can also define named scenarios, selected
// with the X-Mock-Scenario header or the mock_scenario query parameter.
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type response struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
	File   string          `json:"file"`
}

type route struct {
	Host      string              `json:"host"`
	Method    string              `json:"method"`
	Path      string              `json:"path"`
	Scenarios map[string]response `json:"scenarios"`
	response
}

type reply struct {
	status      int
	body        []byte
	contentType string
}

type server struct {
	dir   string
	debug bool
	// hosts are the lowercase hostnames the server answers (SERVER_NAMES); empty means any host.
	hosts map[string]bool
}

func main() {
	dir := flag.String("dir", ".", "project directory (mock.env, routes.json, certs/)")
	httpPort := flag.String("http-port", "", "HTTP port (overrides HTTP_PORT in mock.env)")
	httpsPort := flag.String("https-port", "", "HTTPS port (overrides HTTPS_PORT in mock.env)")
	debug := flag.Bool("debug", false, "log every request in detail: client, TLS, headers and body (or DEBUG=true in mock.env)")
	logPath := flag.String("log-file", "", "write the log to this file, recreating it if it is deleted (default: stderr)")
	flag.Parse()

	if *logPath != "" {
		log.SetOutput(&logFile{path: *logPath})
	}

	env := readEnvFile(filepath.Join(*dir, "mock.env"))
	pick := func(flagValue, key, fallback string) string {
		if flagValue != "" {
			return flagValue
		}
		if v := env[key]; v != "" {
			return v
		}
		return fallback
	}
	cfgHTTP := pick(*httpPort, "HTTP_PORT", "80")
	cfgHTTPS := pick(*httpsPort, "HTTPS_PORT", "443")

	s := &server{dir: *dir, debug: *debug || isTrue(env["DEBUG"]), hosts: parseHosts(env["SERVER_NAMES"])}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)

	cert := filepath.Join(*dir, "certs", "mock.pem")
	key := filepath.Join(*dir, "certs", "mock-key.pem")
	if _, err := tls.LoadX509KeyPair(cert, key); err != nil {
		log.Printf("HTTPS disabled, cannot load certificate: %v", err)
	} else {
		go func() {
			log.Printf("listening on https :%s", cfgHTTPS)
			log.Fatal(http.ListenAndServeTLS(":"+cfgHTTPS, cert, key, mux))
		}()
	}

	log.Printf("listening on http :%s (routes: %s)", cfgHTTP, filepath.Join(*dir, "routes.json"))
	log.Fatal(http.ListenAndServe(":"+cfgHTTP, mux))
}

// logFile is a log writer that reopens its file when it was deleted or replaced (e.g. by a log rotation or a manual
// cleanup), because a file held open by launchd would keep receiving the output after being unlinked.
type logFile struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stale() {
		l.reopen()
	}
	if l.f == nil {
		return os.Stderr.Write(p)
	}
	return l.f.Write(p)
}

// stale reports whether the open file is no longer the one at the path (or nothing is open yet).
func (l *logFile) stale() bool {
	if l.f == nil {
		return true
	}
	onDisk, err := os.Stat(l.path)
	if err != nil {
		return true
	}
	open, err := l.f.Stat()
	return err != nil || !os.SameFile(onDisk, open)
}

// reopen opens (and creates, with its directory) the log file; on failure it keeps writing to stderr.
func (l *logFile) reopen() {
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o755); err != nil {
		return
	}
	if f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		l.f = f
	}
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	reqBody, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	scenario := r.Header.Get("X-Mock-Scenario")
	if scenario == "" {
		scenario = r.URL.Query().Get("mock_scenario")
	}

	if !s.allowsHost(r.Host) {
		if s.debug {
			log.Print(dumpRequest(r, reqBody))
		}
		log.Printf("refused host %q: %s %s (not in SERVER_NAMES)", r.Host, r.Method, r.URL.RequestURI())
		http.Error(w, "unknown host", http.StatusNotFound)
		return
	}

	rep := s.respond(hostName(r.Host), r.Method, r.URL.Path, scenario)
	if s.debug {
		log.Print(dumpRequest(r, reqBody))
	}

	if rep.contentType != "" {
		w.Header().Set("Content-Type", rep.contentType)
	}
	w.WriteHeader(rep.status)
	_, _ = w.Write(rep.body)

	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	log.Printf("%s %s %s scenario=%s status=%d body=%q", scheme, r.Method, r.URL.RequestURI(), scenario, rep.status, reqBody)
}

// dumpRequest renders everything a client sent: client address, protocol, TLS details, headers and body.
func dumpRequest(r *http.Request, body []byte) string {
	var b strings.Builder
	fmt.Fprintf(&b, "debug request: %s %s %s from %s host=%q\n", r.Method, r.URL.RequestURI(), r.Proto, r.RemoteAddr, r.Host)
	if r.TLS != nil {
		fmt.Fprintf(&b, "  tls: %s sni=%q alpn=%q\n", tls.VersionName(r.TLS.Version), r.TLS.ServerName, r.TLS.NegotiatedProtocol)
	}
	names := make([]string, 0, len(r.Header))
	for name := range r.Header {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "  header %s: %s\n", name, strings.Join(r.Header[name], ", "))
	}
	fmt.Fprintf(&b, "  body (%d bytes): %q", len(body), body)
	return b.String()
}

func parseHosts(names string) map[string]bool {
	hosts := map[string]bool{}
	for _, name := range strings.Fields(names) {
		hosts[strings.ToLower(strings.Trim(name, "[]"))] = true
	}
	return hosts
}

// allowsHost reports whether the Host header (port ignored) is one of the declared SERVER_NAMES.
func (s *server) allowsHost(host string) bool {
	if len(s.hosts) == 0 {
		return true
	}
	return s.hosts[hostName(host)]
}

// hostName returns the lowercase hostname of a Host header, without port or IPv6 brackets.
func hostName(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

func isTrue(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// respond picks the first route matching the request, re-reading routes.json on every call.
func (s *server) respond(host, method, path, scenario string) reply {
	routes, err := s.loadRoutes()
	if err != nil {
		return textReply(http.StatusInternalServerError, "shttpmocksrv: %v", err)
	}

	for i := range routes {
		rt := &routes[i]
		if !rt.matches(host, method, path) {
			continue
		}
		resp := rt.response
		if scenario != "" {
			var ok bool
			if resp, ok = rt.Scenarios[scenario]; !ok {
				return textReply(http.StatusInternalServerError, "shttpmocksrv: unknown scenario '%s' for route '%s'", scenario, rt.Path)
			}
		}
		return s.build(resp)
	}
	return textReply(http.StatusNotFound, "shttpmocksrv: no route for %s %s", method, path)
}

func (s *server) loadRoutes() ([]route, error) {
	path := filepath.Join(s.dir, "routes.json")
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read routes.json: %w", err)
	}
	var cfg struct {
		Routes []route `json:"routes"`
	}
	if err := json.Unmarshal(content, &cfg); err != nil {
		return nil, fmt.Errorf("invalid routes.json: %w", err)
	}
	for i, rt := range cfg.Routes {
		if rt.Path == "" {
			return nil, fmt.Errorf("invalid routes.json: route %d has no path", i+1)
		}
	}
	return cfg.Routes, nil
}

func (rt route) matches(host, method, path string) bool {
	if rt.Host != "" && !strings.EqualFold(rt.Host, host) {
		return false
	}
	if rt.Method != "" && !strings.EqualFold(rt.Method, method) {
		return false
	}
	if prefix, ok := strings.CutSuffix(rt.Path, "*"); ok {
		return strings.HasPrefix(path, prefix)
	}
	return rt.Path == path
}

func (s *server) build(resp response) reply {
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	if status < 100 || status > 599 {
		return textReply(http.StatusInternalServerError, "shttpmocksrv: invalid status %d", status)
	}

	switch {
	case len(resp.Body) > 0 && resp.File != "":
		return textReply(http.StatusInternalServerError, "shttpmocksrv: a response sets both body and file")
	case resp.File != "":
		content, err := os.ReadFile(filepath.Join(s.dir, resp.File))
		if err != nil {
			return textReply(http.StatusInternalServerError, "shttpmocksrv: cannot read response file: %v", err)
		}
		return reply{status: status, body: content, contentType: "application/json"}
	case len(resp.Body) > 0:
		return reply{status: status, body: resp.Body, contentType: "application/json"}
	default:
		return reply{status: status}
	}
}

func textReply(status int, format string, args ...any) reply {
	return reply{status: status, body: []byte(fmt.Sprintf(format, args...) + "\n"), contentType: "text/plain; charset=utf-8"}
}

// readEnvFile parses simple KEY=VALUE lines (comments and optional quotes supported).
func readEnvFile(path string) map[string]string {
	values := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return values
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		values[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return values
}
