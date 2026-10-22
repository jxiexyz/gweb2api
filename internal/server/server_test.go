package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
	"github.com/ikhsan3adi/gemini-web2api/internal/format"
	"github.com/ikhsan3adi/gemini-web2api/internal/models"
)

func TestHealthEndpoint(t *testing.T) {
	testVer := "test-version-1.0"
	cfg := config.Default()
	app := New(cfg, testVer)
	handler := app.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Expected status 200, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Invalid JSON response: %v", err)
	}

	if body["status"] != "ok" || body["version"] != testVer {
		t.Errorf("Unexpected health response: %v", body)
	}
}

func TestAuthMatrix(t *testing.T) {
	cfg := config.Default()
	cfg.APIKeys = []string{"sk-secret-key"}
	app := New(cfg, "test-version")
	handler := app.Handler()

	// 1. No key -> 401
	req1 := httptest.NewRequest("GET", "/v1/models", nil)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 without auth, got %d", rec1.Code)
	}

	// 2. Wrong key -> 401
	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2.Header.Set("Authorization", "Bearer wrong-key")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("Expected 401 with wrong key, got %d", rec2.Code)
	}

	// 3. Valid Bearer token -> 200
	req3 := httptest.NewRequest("GET", "/v1/models", nil)
	req3.Header.Set("Authorization", "Bearer sk-secret-key")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("Expected 200 with Bearer token, got %d", rec3.Code)
	}

	// 4. Valid x-api-key header -> 200
	req4 := httptest.NewRequest("GET", "/v1/models", nil)
	req4.Header.Set("x-api-key", "sk-secret-key")
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Errorf("Expected 200 with x-api-key, got %d", rec4.Code)
	}

	// 5. Valid x-goog-api-key header -> 200
	req5 := httptest.NewRequest("GET", "/v1/models", nil)
	req5.Header.Set("x-goog-api-key", "sk-secret-key")
	rec5 := httptest.NewRecorder()
	handler.ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusOK {
		t.Errorf("Expected 200 with x-goog-api-key, got %d", rec5.Code)
	}

	// 6. Valid query param ?key= -> 200
	req6 := httptest.NewRequest("GET", "/v1/models?key=sk-secret-key", nil)
	rec6 := httptest.NewRecorder()
	handler.ServeHTTP(rec6, req6)
	if rec6.Code != http.StatusOK {
		t.Errorf("Expected 200 with ?key= query, got %d", rec6.Code)
	}
}

func TestCORSPreflight(t *testing.T) {
	cfg := config.Default()
	app := New(cfg, "test-version")
	handler := app.Handler()

	req := httptest.NewRequest("OPTIONS", "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("Expected status 204 for OPTIONS, got %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("Expected CORS origin *, got %s", rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestMarshalNoEscapeHTML(t *testing.T) {
	data := map[string]string{
		"text": "<hello & world>",
	}
	b, err := marshalNoEscapeHTML(data)
	if err != nil {
		t.Fatalf("Unexpected error: %v", err)
	}
	expected := `{"text":"<hello & world>"}`
	if string(b) != expected {
		t.Errorf("Got %q, want %q", string(b), expected)
	}
}

func TestChunkedTransferEncoding(t *testing.T) {
	// Verify that the server can parse request bodies via net/http transparent dechunking.
	cfg := config.Default()
	app := New(cfg, "test")
	handler := app.Handler()

	body := `{"model":"gemini-3.6-flash","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should not be 400 (bad request) or 500 — it will be 502 (upstream unreachable) or 200,
	// but not a body-parsing error.
	if rec.Code == http.StatusBadRequest {
		t.Errorf("Handler rejected valid JSON body, got %d", rec.Code)
	}
}

func TestResponsesSequenceNumberOrder(t *testing.T) {
	// Test BuildResponseOutput structure and verify format helpers produce consistent output.
	output := format.BuildResponseOutput("Hello world", nil, "msg_test123")
	if len(output) != 1 {
		t.Fatalf("Expected 1 output item, got %d", len(output))
	}
	item := output[0]
	if item["type"] != "message" {
		t.Errorf("Expected type=message, got %v", item["type"])
	}
	if item["id"] != "msg_test123" {
		t.Errorf("Expected id=msg_test123, got %v", item["id"])
	}

	// With tool calls
	tc := models.OpenAIToolCall{
		ID:   "call_abc123",
		Type: "function",
		Function: models.OpenAIToolCallFunction{
			Name:      "get_weather",
			Arguments: `{"city":"Jakarta"}`,
		},
	}
	outputWithTools := format.BuildResponseOutput("", []models.OpenAIToolCall{tc}, "msg_456")
	// When text is empty, only the function_call item is returned (no empty message).
	if len(outputWithTools) != 1 {
		t.Fatalf("Expected 1 output item (function_call only when text empty), got %d", len(outputWithTools))
	}
	if outputWithTools[0]["type"] != "function_call" {
		t.Errorf("First item should be function_call, got %v", outputWithTools[0]["type"])
	}
	if outputWithTools[0]["call_id"] != "call_abc123" {
		t.Errorf("Expected call_id=call_abc123, got %v", outputWithTools[0]["call_id"])
	}

	// Verify responses format: the non-streaming endpoint should accept valid structure.
	// The responses API uses "input" field, not "messages".
	cfg := config.Default()
	app := New(cfg, "test")
	handler := app.Handler()

	body := `{"model":"gemini-3.6-flash","input":"test prompt"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	// Should not be 400 (bad request) — may be 502 (upstream unreachable)
	if rec.Code == http.StatusBadRequest {
		t.Errorf("Responses endpoint rejected valid JSON, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestModelHeaders(t *testing.T) {
	cfg := config.Default()
	app := New(cfg, "test")
	handler := app.Handler()

	// 1. Known model: gemini-4-argon
	bodyKnown := `{"model":"gemini-4-argon","messages":[{"role":"user","content":"hi"}]}`
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(bodyKnown))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if got := rec1.Header().Get("X-Resolved-Model"); got != "gemini-4-argon" {
		t.Errorf("Expected X-Resolved-Model=gemini-4-argon, got %q", got)
	}
	if got := rec1.Header().Get("X-Model-Fallback"); got != "" {
		t.Errorf("Expected X-Model-Fallback to be empty for known model, got %q", got)
	}

	// 2. Unknown model: should fallback and set X-Model-Fallback: true
	bodyUnknown := `{"model":"gemini-9.9-fake","messages":[{"role":"user","content":"hi"}]}`
	req2 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(bodyUnknown))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if got := rec2.Header().Get("X-Resolved-Model"); got != "gemini-3.6-flash" {
		t.Errorf("Expected X-Resolved-Model=gemini-3.6-flash, got %q", got)
	}
	if got := rec2.Header().Get("X-Model-Fallback"); got != "true" {
		t.Errorf("Expected X-Model-Fallback=true for unknown model, got %q", got)
	}
}

func TestVideoEndpoints(t *testing.T) {
	cfg := config.Default()
	app := New(cfg, "test")
	handler := app.Handler()

	// 1. POST /v1/chat/completions with gemini-video
	bodyVideo := `{"model":"gemini-video","messages":[{"role":"user","content":"Generate a 3 second video of a cat"}]}`
	req1 := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(bodyVideo))
	req1.Header.Set("Content-Type", "application/json")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	if got := rec1.Header().Get("X-Resolved-Model"); got != "gemini-video" {
		t.Errorf("Expected X-Resolved-Model=gemini-video, got %q", got)
	}
	if got := rec1.Header().Get("X-Model-Fallback"); got != "" {
		t.Errorf("Expected X-Model-Fallback to be empty for gemini-video, got %q", got)
	}

	// 2. POST /v1/videos valid video model
	bodyCreate := `{"model":"gemini-video","prompt":"Generate a 3 second video of a cat"}`
	req2 := httptest.NewRequest("POST", "/v1/videos", strings.NewReader(bodyCreate))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("Expected 200 from POST /v1/videos, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var job map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &job); err != nil {
		t.Fatalf("Invalid JSON from POST /v1/videos: %v", err)
	}
	if job["model"] != "gemini-video" {
		t.Errorf("Expected job model=gemini-video, got %v", job["model"])
	}
	if job["status"] != "queued" && job["status"] != "in_progress" {
		t.Errorf("Expected queued or in_progress status, got %v", job["status"])
	}

	// 3. POST /v1/videos with invalid (non-video) model
	bodyInvalidModel := `{"model":"gemini-3.6-flash","prompt":"Generate a video"}`
	req3 := httptest.NewRequest("POST", "/v1/videos", strings.NewReader(bodyInvalidModel))
	req3.Header.Set("Content-Type", "application/json")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)

	if rec3.Code != http.StatusBadRequest {
		t.Errorf("Expected 400 for non-video model in /v1/videos, got %d", rec3.Code)
	}
	if !strings.Contains(rec3.Body.String(), "model must be a video model") {
		t.Errorf("Expected error message 'model must be a video model', got %s", rec3.Body.String())
	}

	// 4. GET /v1/videos/{id} non-existent
	req4 := httptest.NewRequest("GET", "/v1/videos/non_existent_12345", nil)
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)

	if rec4.Code != http.StatusNotFound {
		t.Errorf("Expected 404 for unknown video id, got %d", rec4.Code)
	}
}

