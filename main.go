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
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type response struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
	File   string          `json:"file"`
}

type route struct {
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
	dir string
}

func main() {
	dir := flag.String("dir", ".", "project directory (mock.env, routes.json, certs/)")
	httpPort := flag.String("http-port", "", "HTTP port (overrides HTTP_PORT in mock.env)")
	httpsPort := flag.String("https-port", "", "HTTPS port (overrides HTTPS_PORT in mock.env)")
	flag.Parse()

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

	s := &server{dir: *dir}
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

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	reqBody, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))

	scenario := r.Header.Get("X-Mock-Scenario")
	if scenario == "" {
		scenario = r.URL.Query().Get("mock_scenario")
	}

	rep := s.respond(r.Method, r.URL.Path, scenario)

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

// respond picks the first route matching the request, re-reading routes.json on every call.
func (s *server) respond(method, path, scenario string) reply {
	routes, err := s.loadRoutes()
	if err != nil {
		return textReply(http.StatusInternalServerError, "shttpmocksrv: %v", err)
	}

	for i := range routes {
		rt := &routes[i]
		if !rt.matches(method, path) {
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

func (rt route) matches(method, path string) bool {
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
