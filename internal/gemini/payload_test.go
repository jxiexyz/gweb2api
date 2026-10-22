package gemini

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/ikhsan3adi/gemini-web2api/internal/config"
)

func TestApplyPersistenceFlagsTemporary(t *testing.T) {
	cfg := config.Config{TemporaryChats: true}
	inner := make([]any, GeminiPayloadSize)
	applyPersistenceFlags(inner, cfg)

	if inner[41] == nil {
		t.Fatal("inner[41] should not be nil")
	}
	flag, ok := inner[41].([]any)
	if !ok {
		t.Fatalf("inner[41] should be []any, got %T", inner[41])
	}
	if len(flag) != 1 || flag[0] != 1 {
		t.Errorf("inner[41] = %v, want [1]", flag)
	}
	if inner[45] != 1 {
		t.Errorf("inner[45] = %v, want 1", inner[45])
	}
}

func TestApplyPersistenceFlagsPersistent(t *testing.T) {
	cfg := config.Config{TemporaryChats: false}
	inner := make([]any, GeminiPayloadSize)
	applyPersistenceFlags(inner, cfg)

	flag, ok := inner[41].([]any)
	if !ok {
		t.Fatalf("inner[41] should be []any, got %T", inner[41])
	}
	if len(flag) != 1 || flag[0] != 2 {
		t.Errorf("inner[41] = %v, want [2]", flag)
	}
}

func TestBuildURLWithBLOverride(t *testing.T) {
	cfg := config.Config{
		AuthUser: "",
		GeminiBL: "old-bl",
	}
	url1 := BuildURL(cfg, "")
	if !strings.Contains(url1, "bl=old-bl") {
		t.Errorf("BuildURL with empty override should use cfg.GeminiBL, got %s", url1)
	}

	url2 := BuildURL(cfg, "new-bl")
	if !strings.Contains(url2, "bl=new-bl") {
		t.Errorf("BuildURL with override should use override, got %s", url2)
	}
}

func TestBuildBodyVideo(t *testing.T) {
	cfg := config.Config{}
	body := BuildBody("Generate a video of a cat", 1, 0, nil, nil, cfg, true)

	// Decode form
	vals, err := url.ParseQuery(body)
	if err != nil {
		t.Fatalf("Failed to parse query: %v", err)
	}
	freq := vals.Get("f.req")
	if freq == "" {
		t.Fatal("Missing f.req in body")
	}

	var outer []any
	if err := json.Unmarshal([]byte(freq), &outer); err != nil {
		t.Fatalf("Failed to unmarshal outer JSON: %v", err)
	}
	if len(outer) < 2 {
		t.Fatalf("Outer array too short: %v", outer)
	}
	innerStr, ok := outer[1].(string)
	if !ok {
		t.Fatalf("inner is not string: %T", outer[1])
	}

	var inner []any
	if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
		t.Fatalf("Failed to unmarshal inner JSON: %v", err)
	}

	// Verify [17] = [[0]]
	t17, ok := inner[17].([]any)
	if !ok || len(t17) != 1 {
		t.Fatalf("inner[17] = %v, want [[0]]", inner[17])
	}
	t17sub, ok := t17[0].([]any)
	if !ok || len(t17sub) != 1 || t17sub[0] != float64(0) {
		t.Fatalf("inner[17] = %v, want [[0]]", inner[17])
	}

	// Verify [30] = [4, 16, 19]
	t30, ok := inner[30].([]any)
	if !ok || len(t30) != 3 || t30[0] != float64(4) || t30[1] != float64(16) || t30[2] != float64(19) {
		t.Fatalf("inner[30] = %v, want [4, 16, 19]", inner[30])
	}

	// Verify [41] = [1]
	t41, ok := inner[41].([]any)
	if !ok || len(t41) != 1 || t41[0] != float64(1) {
		t.Fatalf("inner[41] = %v, want [1]", inner[41])
	}

	// Verify [79] = 1
	if inner[79] != float64(1) {
		t.Fatalf("inner[79] = %v, want 1", inner[79])
	}

	// Verify [80] = 1
	if inner[80] != float64(1) {
		t.Fatalf("inner[80] = %v, want 1", inner[80])
	}

	// Verify [91] = 1
	if inner[91] != float64(1) {
		t.Fatalf("inner[91] = %v, want 1", inner[91])
	}

	// Verify [98] = 1
	if inner[98] != float64(1) {
		t.Fatalf("inner[98] = %v, want 1", inner[98])
	}
}

func TestBuildVideoHeader(t *testing.T) {
	uuid := "TEST-UUID-1234"
	bl := "boq_gemini-web-uiserver_20261022.01_p0"
	hdr := BuildVideoHeader(uuid, bl)

	expected := `[1,null,null,null,null,null,null,0,[4,5,6,8,16,19,20,4,5,6,8,16,19,20],null,null,4,null,null,1,1,"TEST-UUID-1234",["boq_gemini-web-uiserver_20261022.01_p0"],null,null]`
	if hdr != expected {
		t.Errorf("BuildVideoHeader got:\n%s\nwant:\n%s", hdr, expected)
	}
}

