package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// newServer returns a server whose directory holds the given files (path relative to the directory -> content).
func newServer(t *testing.T, files map[string]string) *server {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &server{dir: dir}
}

func do(s *server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.handle(rec, req)
	return rec
}

func get(s *server, target string) *httptest.ResponseRecorder {
	return do(s, httptest.NewRequest("GET", target, nil))
}

func TestGivenCatchAllRoute_WhenAnyRequest_ThenReturnInlineBodyAsIs(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{
		"routes.json": `{"routes": [{"path": "/*", "body": {"success": true, "uses": 5}}]}`,
	})

	for _, method := range []string{"GET", "POST", "DELETE"} {
		// WHEN
		rec := do(s, httptest.NewRequest(method, "/api/items", strings.NewReader("name=abc")))

		// THEN
		if rec.Code != 200 {
			t.Errorf("%s: status = %d, want 200", method, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: content type = %q, want application/json", method, got)
		}
		if rec.Body.String() != `{"success": true, "uses": 5}` {
			t.Errorf("%s: body = %q", method, rec.Body.String())
		}
	}
}

func TestGivenSeveralRoutes_WhenRequest_ThenEachUrlGetsItsOwnResponse(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{
		"routes.json": `{"routes": [
			{"path": "/api/items", "body": {"from": "items"}},
			{"path": "/health", "body": {"from": "health"}},
			{"path": "/admin/*", "body": {"from": "admin"}},
			{"path": "/*", "body": {"from": "fallback"}}
		]}`,
	})

	for target, want := range map[string]string{
		"/api/items":       `{"from": "items"}`,
		"/api/items?x=1":   `{"from": "items"}`,
		"/health":          `{"from": "health"}`,
		"/admin":           `{"from": "fallback"}`,
		"/admin/users/1":   `{"from": "admin"}`,
		"/api/items/extra": `{"from": "fallback"}`,
		"/something/else":  `{"from": "fallback"}`,
	} {
		// WHEN
		got := get(s, target).Body.String()

		// THEN
		if got != want {
			t.Errorf("%s: body = %q, want %q", target, got, want)
		}
	}
}

func TestGivenRouteWithMethod_WhenOtherMethod_ThenFallThroughToNextRoute(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{
		"routes.json": `{"routes": [
			{"method": "post", "path": "/items", "body": {"from": "post"}},
			{"path": "/items", "status": 405, "body": {"from": "other"}}
		]}`,
	})

	// WHEN / THEN
	if rec := do(s, httptest.NewRequest("POST", "/items", nil)); rec.Body.String() != `{"from": "post"}` {
		t.Errorf("POST: body = %q", rec.Body.String())
	}
	rec := get(s, "/items")
	if rec.Code != 405 || rec.Body.String() != `{"from": "other"}` {
		t.Errorf("GET: status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestGivenNoMatchingRoute_WhenRequest_ThenReturn404Text(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/only", "body": {}}]}`})

	// WHEN
	rec := get(s, "/other")

	// THEN
	if rec.Code != 404 {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") || !strings.Contains(rec.Body.String(), "no route for GET /other") {
		t.Errorf("content type = %q, body = %q", rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestGivenStatusAndNoBody_WhenRequest_ThenReturnStatusWithEmptyBody(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/*", "status": 204}]}`})

	// WHEN
	rec := get(s, "/")

	// THEN
	if rec.Code != 204 || rec.Body.Len() != 0 {
		t.Errorf("status = %d, body = %q, want 204 and empty body", rec.Code, rec.Body.String())
	}
}

func TestGivenRouteWithFile_WhenRequest_ThenServeFileRelativeToDirectory(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{
		"routes.json":       `{"routes": [{"path": "/big", "status": 201, "file": "payloads/big.json"}]}`,
		"payloads/big.json": "{\n  \"big\": true\n}\n",
	})

	// WHEN
	rec := get(s, "/big")

	// THEN
	if rec.Code != 201 || rec.Body.String() != "{\n  \"big\": true\n}\n" {
		t.Errorf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestGivenBrokenResponses_WhenRequest_ThenReturn500WithReason(t *testing.T) {
	for name, tc := range map[string]struct{ routes, want string }{
		"missing file":       {`{"routes": [{"path": "/*", "file": "nope.json"}]}`, "cannot read response file"},
		"body and file":      {`{"routes": [{"path": "/*", "body": {}, "file": "x.json"}]}`, "both body and file"},
		"invalid status":     {`{"routes": [{"path": "/*", "status": 999, "body": {}}]}`, "invalid status 999"},
		"route without path": {`{"routes": [{"body": {}}]}`, "route 1 has no path"},
		"malformed json":     {`{"routes": [`, "invalid routes.json"},
	} {
		// GIVEN
		s := newServer(t, map[string]string{"routes.json": tc.routes})

		// WHEN
		rec := get(s, "/")

		// THEN
		if rec.Code != 500 || !strings.Contains(rec.Body.String(), tc.want) {
			t.Errorf("%s: status = %d, body = %q, want 500 containing %q", name, rec.Code, rec.Body.String(), tc.want)
		}
	}
}

func TestGivenMissingRoutesFile_WhenRequest_ThenReturn500(t *testing.T) {
	// GIVEN
	s := newServer(t, nil)

	// WHEN
	rec := get(s, "/")

	// THEN
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "cannot read routes.json") {
		t.Errorf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestGivenScenarios_WhenRequest_ThenHeaderWinsOverQueryOverRouteDefault(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{
		"routes.json": `{"routes": [{"path": "/items", "body": {"from": "default"}, "scenarios": {
			"refunded": {"body": {"from": "refunded"}},
			"invalid": {"status": 404, "body": {"from": "invalid"}}
		}}]}`,
	})

	// WHEN / THEN
	req := httptest.NewRequest("GET", "/items?mock_scenario=invalid", nil)
	req.Header.Set("X-Mock-Scenario", "refunded")
	if got := do(s, req).Body.String(); got != `{"from": "refunded"}` {
		t.Errorf("header + query: body = %q, want refunded", got)
	}

	rec := get(s, "/items?mock_scenario=invalid")
	if rec.Code != 404 || rec.Body.String() != `{"from": "invalid"}` {
		t.Errorf("query only: status = %d, body = %q, want 404 invalid", rec.Code, rec.Body.String())
	}

	if got := get(s, "/items").Body.String(); got != `{"from": "default"}` {
		t.Errorf("none: body = %q, want default", got)
	}
}

func TestGivenUnknownScenario_WhenRequest_ThenReturn500(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/items", "body": {}}]}`})
	req := httptest.NewRequest("GET", "/items", nil)
	req.Header.Set("X-Mock-Scenario", "nope")

	// WHEN
	rec := do(s, req)

	// THEN
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "unknown scenario 'nope' for route '/items'") {
		t.Errorf("status = %d, body = %q", rec.Code, rec.Body.String())
	}
}

func TestGivenRoutesEditedBetweenRequests_WhenRequest_ThenServeNewContent(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/*", "body": {"uses": 5}}]}`})
	if got := get(s, "/").Body.String(); got != `{"uses": 5}` {
		t.Fatalf("initial body = %q", got)
	}

	// WHEN
	edited := `{"routes": [{"path": "/*", "body": {"uses": 99}}]}`
	if err := os.WriteFile(filepath.Join(s.dir, "routes.json"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	// THEN
	if got := get(s, "/").Body.String(); got != `{"uses": 99}` {
		t.Errorf("body after edit = %q, want {\"uses\": 99}", got)
	}
}

func TestGivenEnvFile_WhenRead_ThenParseValuesCommentsAndQuotes(t *testing.T) {
	// GIVEN
	path := filepath.Join(t.TempDir(), "mock.env")
	content := "# comment\n\nHTTP_PORT=8080\nSERVER_NAMES=\"localhost api.example.com\"\nOTHER = value\nnot a pair\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// WHEN
	values := readEnvFile(path)

	// THEN
	want := map[string]string{
		"HTTP_PORT":    "8080",
		"SERVER_NAMES": "localhost api.example.com",
		"OTHER":        "value",
	}
	if len(values) != len(want) {
		t.Errorf("values = %v, want %v", values, want)
	}
	for k, v := range want {
		if values[k] != v {
			t.Errorf("%s = %q, want %q", k, values[k], v)
		}
	}
}

func TestGivenMissingEnvFile_WhenRead_ThenReturnEmpty(t *testing.T) {
	// WHEN
	values := readEnvFile(filepath.Join(t.TempDir(), "absent.env"))

	// THEN
	if len(values) != 0 {
		t.Errorf("values = %v, want empty", values)
	}
}

func TestGivenDebugMode_WhenRequest_ThenLogHeadersAndBody(t *testing.T) {
	// GIVEN
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(io.Discard)
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/*", "body": {}}]}`})
	s.debug = true
	req := httptest.NewRequest("POST", "/api/items?a=1", strings.NewReader("name=abc"))
	req.Header.Set("X-Trace", "t1")

	// WHEN
	do(s, req)

	// THEN
	for _, want := range []string{"debug request: POST /api/items?a=1", "header X-Trace: t1", `body (8 bytes): "name=abc"`} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q does not contain %q", logs.String(), want)
		}
	}
}

func TestGivenNoDebugMode_WhenRequest_ThenNoDetailedLog(t *testing.T) {
	// GIVEN
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(io.Discard)
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/*", "body": {}}]}`})

	// WHEN
	get(s, "/x")

	// THEN
	if strings.Contains(logs.String(), "debug request") {
		t.Errorf("unexpected debug log: %q", logs.String())
	}
}

func TestGivenServerNames_WhenRequestOtherHost_ThenRefuse(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/*", "body": {}}]}`})
	s.hosts = parseHosts("api.example.com 127.0.0.1 ::1")

	tests := []struct {
		host string
		want int
	}{
		{"api.example.com", 200},
		{"API.example.com:8443", 200},
		{"127.0.0.1:80", 200},
		{"[::1]:443", 200},
		{"localhost", 404},
		{"localhost:80", 404},
		{"other.example.com", 404},
	}
	for _, tt := range tests {
		// WHEN
		req := httptest.NewRequest("GET", "/x", nil)
		req.Host = tt.host
		rec := do(s, req)

		// THEN
		if rec.Code != tt.want {
			t.Errorf("host %q: status = %d, want %d", tt.host, rec.Code, tt.want)
		}
	}
}

func TestGivenNoServerNames_WhenRequestAnyHost_ThenAnswer(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{"routes.json": `{"routes": [{"path": "/*", "body": {}}]}`})

	// WHEN
	req := httptest.NewRequest("GET", "/x", nil)
	req.Host = "localhost"
	rec := do(s, req)

	// THEN
	if rec.Code != 200 {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestGivenRouteWithHost_WhenRequest_ThenMatchOnlyThatHost(t *testing.T) {
	// GIVEN
	s := newServer(t, map[string]string{"routes.json": `{"routes": [
		{"host": "test.toto.com", "path": "/api/v2/countries", "body": {"from": "toto"}},
		{"path": "/*", "body": {"from": "any"}}
	]}`})

	tests := []struct {
		host string
		want string
	}{
		{"test.toto.com", `{"from": "toto"}`},
		{"TEST.toto.com:8443", `{"from": "toto"}`},
		{"api.example.com", `{"from": "any"}`},
	}
	for _, tt := range tests {
		// WHEN
		req := httptest.NewRequest("GET", "/api/v2/countries", nil)
		req.Host = tt.host
		rec := do(s, req)

		// THEN
		if got := rec.Body.String(); got != tt.want {
			t.Errorf("host %q: body = %q, want %q", tt.host, got, tt.want)
		}
	}
}

func TestGivenDeletedLogFile_WhenWrite_ThenRecreateIt(t *testing.T) {
	// GIVEN
	path := filepath.Join(t.TempDir(), "logs", "server.log")
	l := &logFile{path: path}
	if _, err := l.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// WHEN
	if _, err := l.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}

	// THEN
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second\n" {
		t.Errorf("log = %q, want %q", content, "second\n")
	}
}
