package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVersion(t *testing.T) {
	s := NewServer(nil, nil)
	req := httptest.NewRequest("GET", "/version", nil)
	rec := httptest.NewRecorder()
	s.handleVersion(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("want status 200, got %d", rec.Code)
	}

	var mapObj map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &mapObj); err != nil {
		t.Fatal(err)
	}

	version, ok := mapObj["version"]
	if !ok || version != "1.2.3" {
		t.Errorf("want version \"1.2.3\", got %q", version)
	}
}
