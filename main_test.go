package main

import (
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
