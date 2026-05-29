package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/georgehuang/admiral/internal/store"
)

// driveServer feeds requests as newline-delimited JSON-RPC into the
// server and returns the responses in the same order. Used by every
// protocol-level test so the MCP wire format is exercised end-to-end,
// not just the handler functions.
func driveServer(t *testing.T, db *store.Store, requests []map[string]any) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, r := range requests {
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		in.Write(body)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	var errBuf bytes.Buffer

	tools := BuildTools(db)
	srv := NewServer(&in, &out, &errBuf, tools)
	if err := srv.Run(context.Background()); err != nil {
		t.Fatalf("server run: %v (stderr: %s)", err, errBuf.String())
	}

	// stdout is newline-delimited JSON objects; one per response.
	var results []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		results = append(results, m)
	}
	return results
}

func newPlannerTestStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestInitialize_AdvertisesToolsCapability(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}},
	})
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	r := resps[0]
	result := r["result"].(map[string]any)
	if result["protocolVersion"] != MCPProtocolVersion {
		t.Fatalf("wrong protocol version: %v", result["protocolVersion"])
	}
	caps := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatal("initialize must advertise tools capability")
	}
	info := result["serverInfo"].(map[string]any)
	if info["name"] != "admiral-planner-mcp" {
		t.Fatalf("wrong server name: %v", info["name"])
	}
}

func TestNotificationsInitialized_NoResponse(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
	})
	if len(resps) != 0 {
		t.Fatalf("notifications must not produce a response, got %d", len(resps))
	}
}

func TestToolsList_ReturnsRegisteredTools(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
	})
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	result := resps[0]["result"].(map[string]any)
	tools := result["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("expected at least one tool")
	}
	found := false
	for _, raw := range tools {
		tool := raw.(map[string]any)
		if tool["name"] == "feature_get_materials" {
			found = true
			if tool["description"] == "" {
				t.Fatalf("tool %q missing description", tool["name"])
			}
			if tool["inputSchema"] == nil {
				t.Fatalf("tool %q missing inputSchema", tool["name"])
			}
		}
	}
	if !found {
		t.Fatal("feature_get_materials not in tools/list output")
	}
}

func TestUnknownMethod_ReturnsMethodNotFound(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "totally/made/up"},
	})
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	errObj := resps[0]["error"].(map[string]any)
	// JSON unmarshals numbers as float64 by default.
	if int(errObj["code"].(float64)) != errCodeMethodNotFound {
		t.Fatalf("wrong code: %v", errObj["code"])
	}
}

func TestPing_ReturnsEmptyResult(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{
		{"jsonrpc": "2.0", "id": 7, "method": "ping"},
	})
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	if _, ok := resps[0]["result"]; !ok {
		t.Fatalf("ping should produce a result, got %+v", resps[0])
	}
}

// --- feature_get_materials tool ---

func TestFeatureGetMaterials_HappyPath(t *testing.T) {
	db := newPlannerTestStore(t)
	// Seed a feature + 2 issues.
	feat := store.Feature{
		ID: "f-1", Name: "login", LinearProjectID: "p-1",
		RequirementsText: "build login",
	}
	issues := []store.FeatureIssue{
		{LinearIssueID: "i-1", AcceptanceCriteria: "email regex"},
		{LinearIssueID: "i-2", AcceptanceCriteria: "session cookie"},
	}
	if err := db.InsertFeatureWithIssues(feat, issues); err != nil {
		t.Fatalf("seed: %v", err)
	}

	resps := driveServer(t, db, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "feature_get_materials",
			"arguments": map[string]any{"feature_id": "f-1"},
		},
	}})
	if len(resps) != 1 {
		t.Fatalf("want 1 response, got %d", len(resps))
	}
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("expected isError=false, got %v", result["isError"])
	}
	// Tool result is encoded as a text content block whose body is
	// itself JSON — the host agent decodes that on its side.
	content := result["content"].([]any)
	textBlock := content[0].(map[string]any)
	if textBlock["type"] != "text" {
		t.Fatalf("expected text block, got %v", textBlock["type"])
	}
	var payload featureGetMaterialsResult
	if err := json.Unmarshal([]byte(textBlock["text"].(string)), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Feature == nil || payload.Feature.ID != "f-1" {
		t.Fatalf("unexpected feature: %+v", payload.Feature)
	}
	if payload.Feature.RequirementsText != "build login" {
		t.Fatalf("requirements lost: %q", payload.Feature.RequirementsText)
	}
	if len(payload.Issues) != 2 {
		t.Fatalf("want 2 issues, got %d", len(payload.Issues))
	}
}

func TestFeatureGetMaterials_MissingFeatureID_ToolError(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "feature_get_materials",
			"arguments": map[string]any{}, // no feature_id
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected isError=true, got %v", result["isError"])
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "feature_id") {
		t.Fatalf("error should mention feature_id, got: %s", text)
	}
}

func TestFeatureGetMaterials_NotFound_ToolError(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "feature_get_materials",
			"arguments": map[string]any{"feature_id": "nope"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError=true for missing feature")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "not found") {
		t.Fatalf("error should say 'not found', got: %s", text)
	}
}

func TestToolsCall_UnknownTool(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "no_such_tool",
			"arguments": map[string]any{},
		},
	}})
	errObj := resps[0]["error"].(map[string]any)
	if int(errObj["code"].(float64)) != errCodeMethodNotFound {
		t.Fatalf("wrong code: %v", errObj["code"])
	}
	if !strings.Contains(errObj["message"].(string), "no_such_tool") {
		t.Fatalf("error should mention tool name, got: %v", errObj["message"])
	}
}

// --- multi-request transcript ---

func TestFullTranscript_InitializeThenListThenCall(t *testing.T) {
	// A realistic host-agent transcript: initialize → notifications/initialized
	// → tools/list → tools/call. All in one connection.
	db := newPlannerTestStore(t)
	_ = db.InsertFeatureWithIssues(
		store.Feature{ID: "f-tx", Name: "tx", LinearProjectID: "p-tx", RequirementsText: "r"},
		[]store.FeatureIssue{{LinearIssueID: "i-1", AcceptanceCriteria: "c"}},
	)
	resps := driveServer(t, db, []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}},
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
		{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{
			"name":      "feature_get_materials",
			"arguments": map[string]any{"feature_id": "f-tx"},
		}},
	})
	// 1 (init) + 0 (notification) + 1 (list) + 1 (call) = 3 responses.
	if len(resps) != 3 {
		t.Fatalf("want 3 responses (notification suppressed), got %d", len(resps))
	}
	// IDs must round-trip in order.
	ids := []int{}
	for _, r := range resps {
		if id, ok := r["id"]; ok {
			ids = append(ids, int(id.(float64)))
		}
	}
	want := []int{1, 2, 3}
	if len(ids) != 3 || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("ids out of order: got %v want %v", ids, want)
	}
}
