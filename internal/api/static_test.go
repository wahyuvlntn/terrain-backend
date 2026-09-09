package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSingleFrontendRoutes(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("Digital Twin AFDD"), 0600); err != nil {
		t.Fatal(err)
	}
	handler := spaHandler(dir)
	for _, agent := range []string{"Desktop", "Mobile"} {
		req := httptest.NewRequest(http.MethodGet, "/digital-twin", nil)
		req.Header.Set("User-Agent", agent)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Digital Twin AFDD") {
			t.Fatalf("%s: status %d, body %s", agent, response.Code, response.Body.String())
		}
	}
	for _, route := range []string{"/api/missing", "/assets/missing.js"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, route, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("%s returned %d, expected 404", route, response.Code)
		}
	}
}
