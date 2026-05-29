package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/georgehuang/admiral/internal/store"
)

// stubGH is the test double for the GitHub client. Implements both
// halves of GitHubClient — GetDiff returns canned diffs / errors,
// PostReview records every call so tests can assert what was sent.
type stubGH struct {
	diffs       map[string]string
	diffErrs    map[string]error
	reviewErrs  map[string]error // per-pr error override for PostReview
	postedRevs  []postedReview
}

type postedReview struct {
	prURL   string
	verdict string
	body    string
}

func (s *stubGH) GetDiff(_ context.Context, prURL string) (string, error) {
	if err, ok := s.diffErrs[prURL]; ok {
		return "", err
	}
	if d, ok := s.diffs[prURL]; ok {
		return d, nil
	}
	return "", errors.New("stubGH: no diff seeded for " + prURL)
}

func (s *stubGH) PostReview(_ context.Context, prURL, verdict, body string) error {
	if err, ok := s.reviewErrs[prURL]; ok {
		return err
	}
	s.postedRevs = append(s.postedRevs, postedReview{prURL, verdict, body})
	return nil
}

// driveServer feeds requests as newline-delimited JSON-RPC into the
// server and returns the responses in the same order. Used by every
// protocol-level test so the MCP wire format is exercised end-to-end,
// not just the handler functions. gh may be nil to simulate a server
// that booted without ADMIRAL_GH_TOKEN.
func driveServer(t *testing.T, db *store.Store, gh GitHubClient, requests []map[string]any) []map[string]any {
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

	tools := BuildTools(db, gh)
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
	resps := driveServer(t, db, nil, []map[string]any{
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
	resps := driveServer(t, db, nil, []map[string]any{
		{"jsonrpc": "2.0", "method": "notifications/initialized"},
	})
	if len(resps) != 0 {
		t.Fatalf("notifications must not produce a response, got %d", len(resps))
	}
}

func TestToolsList_ReturnsRegisteredTools(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, nil, []map[string]any{
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
	resps := driveServer(t, db, nil, []map[string]any{
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
	resps := driveServer(t, db, nil, []map[string]any{
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

	resps := driveServer(t, db, nil, []map[string]any{{
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
	resps := driveServer(t, db, nil, []map[string]any{{
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
	resps := driveServer(t, db, nil, []map[string]any{{
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
	resps := driveServer(t, db, nil, []map[string]any{{
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
	resps := driveServer(t, db, nil, []map[string]any{
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

// --- issue_list_by_feature ---

// seedFeatureWithTask seeds a feature + one issue + (optionally) an
// admiral_tasks row claiming that issue so tests can verify the join
// in issue_list_by_feature.
func seedFeatureWithTask(t *testing.T, db *store.Store, featureID, issueID, identifier, prURL, state string) {
	t.Helper()
	if err := db.InsertFeatureWithIssues(
		store.Feature{ID: featureID, Name: featureID, LinearProjectID: "p-" + featureID, RequirementsText: "r"},
		[]store.FeatureIssue{{LinearIssueID: issueID, AcceptanceCriteria: "criteria-for-" + issueID}},
	); err != nil {
		t.Fatalf("seed feature: %v", err)
	}
	if identifier == "" {
		return // no admiral_task wanted
	}
	if _, err := db.ClaimAdmiralTask(issueID, identifier, "ev-1"); err != nil {
		t.Fatalf("claim task: %v", err)
	}
	if err := db.UpdateAdmiralTask(issueID, func(at *store.AdmiralTask) {
		at.State = state
		at.PRURL = prURL
		at.Branch = "linear/" + identifier
	}); err != nil {
		t.Fatalf("update task: %v", err)
	}
}

func TestIssueListByFeature_JoinsAdmiralTaskState(t *testing.T) {
	db := newPlannerTestStore(t)
	seedFeatureWithTask(t, db, "f-1", "i-1", "GEO-50",
		"https://github.com/o/r/pull/1", store.JobStateDone)

	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "issue_list_by_feature",
			"arguments": map[string]any{"feature_id": "f-1"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError true: %v", result["content"])
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload issueListByFeatureResult
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.FeatureID != "f-1" || len(payload.Issues) != 1 {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	got := payload.Issues[0]
	if got.LinearIssueID != "i-1" || got.IssueIdentifier != "GEO-50" ||
		got.PRURL != "https://github.com/o/r/pull/1" || got.State != store.JobStateDone ||
		got.AcceptanceCriteria != "criteria-for-i-1" {
		t.Fatalf("row missing fields: %+v", got)
	}
}

func TestIssueListByFeature_IssueWithoutAdmiralTask(t *testing.T) {
	// Issue is registered in planner but admiral hasn't started it yet.
	// State / PRURL must be empty; LinearIssueID + criteria still present.
	db := newPlannerTestStore(t)
	seedFeatureWithTask(t, db, "f-2", "i-orphan", "", "", "")

	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "issue_list_by_feature",
			"arguments": map[string]any{"feature_id": "f-2"},
		},
	}})
	text := resps[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload issueListByFeatureResult
	_ = json.Unmarshal([]byte(text), &payload)
	if len(payload.Issues) != 1 {
		t.Fatalf("want 1 issue, got %d", len(payload.Issues))
	}
	got := payload.Issues[0]
	if got.State != "" || got.PRURL != "" {
		t.Fatalf("expected empty state/pr_url for unstarted issue, got %+v", got)
	}
	if got.AcceptanceCriteria == "" {
		t.Fatal("criteria should still be present")
	}
}

func TestIssueListByFeature_UnknownFeature(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "issue_list_by_feature",
			"arguments": map[string]any{"feature_id": "nope"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError for unknown feature")
	}
}

// --- pr_get_materials ---

func TestPRGetMaterials_HappyPath(t *testing.T) {
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/42"
	seedFeatureWithTask(t, db, "f-pr", "i-pr", "GEO-42", prURL, store.JobStateDone)
	gh := &stubGH{
		diffs: map[string]string{prURL: "diff --git a/x b/x\n+hello"},
	}

	resps := driveServer(t, db, gh, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "pr_get_materials",
			"arguments": map[string]any{"pr_url": prURL},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError true: %v", result["content"])
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload prGetMaterialsResult
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.FeatureID != "f-pr" || payload.LinearIssueID != "i-pr" ||
		payload.IssueIdentifier != "GEO-42" ||
		payload.AcceptanceCriteria != "criteria-for-i-pr" ||
		payload.Branch != "linear/GEO-42" ||
		!strings.Contains(payload.Diff, "+hello") {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

func TestPRGetMaterials_NoGHClient_ReturnsError(t *testing.T) {
	// Server booted without ADMIRAL_GH_TOKEN — pr_get_materials must
	// fail cleanly with a guidance message rather than panic.
	db := newPlannerTestStore(t)
	seedFeatureWithTask(t, db, "f", "i", "X-1", "https://github.com/o/r/pull/1", store.JobStateDone)

	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "pr_get_materials",
			"arguments": map[string]any{"pr_url": "https://github.com/o/r/pull/1"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError when gh client missing")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "ADMIRAL_GH_TOKEN") {
		t.Fatalf("error should mention ADMIRAL_GH_TOKEN, got: %s", text)
	}
}

func TestPRGetMaterials_UntrackedPR(t *testing.T) {
	db := newPlannerTestStore(t)
	gh := &stubGH{}
	resps := driveServer(t, db, gh, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "pr_get_materials",
			"arguments": map[string]any{"pr_url": "https://github.com/o/r/pull/999"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError for PR admiral doesn't track")
	}
}

func TestPRGetMaterials_IssueNotInFeature(t *testing.T) {
	// admiral has a task with this PR but the underlying issue was
	// never registered with the planner — common case for issues
	// created before planner-mcp was rolled out.
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/77"
	if _, err := db.ClaimAdmiralTask("legacy-issue", "GEO-77", "ev"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = db.UpdateAdmiralTask("legacy-issue", func(at *store.AdmiralTask) {
		at.State = store.JobStateDone
		at.PRURL = prURL
		at.Branch = "linear/GEO-77"
	})
	gh := &stubGH{diffs: map[string]string{prURL: "diff"}}

	resps := driveServer(t, db, gh, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "pr_get_materials",
			"arguments": map[string]any{"pr_url": prURL},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError for legacy issue not in planner")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "not part of any planner feature") {
		t.Fatalf("error wording lost guidance: %s", text)
	}
}

func TestPRGetMaterials_DiffFetchFails(t *testing.T) {
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/88"
	seedFeatureWithTask(t, db, "f-x", "i-x", "GEO-88", prURL, store.JobStateDone)
	gh := &stubGH{diffErrs: map[string]error{prURL: errors.New("rate-limited")}}

	resps := driveServer(t, db, gh, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "pr_get_materials",
			"arguments": map[string]any{"pr_url": prURL},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError when gh.GetDiff fails")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "rate-limited") {
		t.Fatalf("error should propagate gh failure, got: %s", text)
	}
}

func TestToolsList_IncludesAllRegisteredTools(t *testing.T) {
	// Belt for the registry: as new tools are added, this catches
	// accidentally dropping one from BuildTools.
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, nil, []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "tools/list"},
	})
	tools := resps[0]["result"].(map[string]any)["tools"].([]any)
	got := map[string]bool{}
	for _, raw := range tools {
		got[raw.(map[string]any)["name"].(string)] = true
	}
	for _, name := range []string{
		"feature_start",
		"issue_set_acceptance",
		"feature_get_materials",
		"issue_list_by_feature",
		"pr_get_materials",
		"pr_verify_submit",
		"feature_close",
	} {
		if !got[name] {
			t.Fatalf("tools/list missing %s; got %v", name, got)
		}
	}
}

// --- feature_start ---

// callFeatureStart is a convenience that decodes the wire envelope
// into the typed result. Returns the result map and tool-level
// (isError + text) info; protocol-level errors fail the test.
func callFeatureStart(t *testing.T, db *store.Store, args map[string]any) (map[string]any, bool, string) {
	t.Helper()
	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "feature_start", "arguments": args},
	}})
	result := resps[0]["result"].(map[string]any)
	isErr := result["isError"].(bool)
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	return result, isErr, text
}

func TestFeatureStart_HappyPath(t *testing.T) {
	db := newPlannerTestStore(t)
	result, isErr, text := callFeatureStart(t, db, map[string]any{
		"name":              "login",
		"requirements_text": "build login with email + password",
		"linear_project_id": "proj-1",
		"source_agent":      "claude",
	})
	if isErr {
		t.Fatalf("happy path failed: %s", text)
	}
	var payload featureStartResult
	if err := json.Unmarshal([]byte(result["content"].([]any)[0].(map[string]any)["text"].(string)), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.HasPrefix(payload.FeatureID, "f-") {
		t.Fatalf("feature_id should start with 'f-', got %q", payload.FeatureID)
	}
	if payload.LinearProjectID != "proj-1" {
		t.Fatalf("linear_project_id round-trip lost: %q", payload.LinearProjectID)
	}
	// Verify the row actually landed in the DB with source_agent + requirements.
	f, _ := db.GetFeature(payload.FeatureID)
	if f == nil || f.RequirementsText != "build login with email + password" || f.SourceAgent != "claude" {
		t.Fatalf("DB row missing fields: %+v", f)
	}
}

func TestFeatureStart_RejectsMissingFields(t *testing.T) {
	db := newPlannerTestStore(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"no name", map[string]any{"requirements_text": "r", "linear_project_id": "p"}},
		{"no requirements", map[string]any{"name": "n", "linear_project_id": "p"}},
		{"no project", map[string]any{"name": "n", "requirements_text": "r"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, isErr, _ := callFeatureStart(t, db, tc.args)
			if !isErr {
				t.Fatal("expected isError")
			}
		})
	}
}

func TestFeatureStart_DuplicateProjectSurfacesExistingFeature(t *testing.T) {
	// Calling feature_start twice with the same linear_project_id must
	// produce an actionable error that names the existing feature, so
	// the host agent can switch to feature_get_materials instead of
	// retrying.
	db := newPlannerTestStore(t)
	_, isErr, text := callFeatureStart(t, db, map[string]any{
		"name": "first", "requirements_text": "r1", "linear_project_id": "p-shared",
	})
	if isErr {
		t.Fatalf("first call: %s", text)
	}
	_, isErr, text = callFeatureStart(t, db, map[string]any{
		"name": "second", "requirements_text": "r2", "linear_project_id": "p-shared",
	})
	if !isErr {
		t.Fatal("expected isError on duplicate project")
	}
	if !strings.Contains(text, "already bound") || !strings.Contains(text, "p-shared") {
		t.Fatalf("error should name the conflicting project, got: %s", text)
	}
}

// --- issue_set_acceptance ---

func TestIssueSetAcceptance_HappyPath(t *testing.T) {
	db := newPlannerTestStore(t)
	_ = db.InsertFeature(store.Feature{ID: "f-1", Name: "a", LinearProjectID: "p-1"})

	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "issue_set_acceptance",
			"arguments": map[string]any{
				"feature_id":          "f-1",
				"linear_issue_id":     "i-1",
				"acceptance_criteria": "Reject invalid emails with 400",
			},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError true: %v", result["content"])
	}
	got, _ := db.GetFeatureIssue("f-1", "i-1")
	if got == nil || got.AcceptanceCriteria != "Reject invalid emails with 400" {
		t.Fatalf("criteria not persisted: %+v", got)
	}
}

func TestIssueSetAcceptance_RefinesCriteria(t *testing.T) {
	// Re-call with new criteria text must overwrite (idempotent upsert).
	db := newPlannerTestStore(t)
	_ = db.InsertFeature(store.Feature{ID: "f-1", Name: "a", LinearProjectID: "p-1"})

	for _, txt := range []string{"v1 criteria", "v2 criteria (refined)"} {
		resps := driveServer(t, db, nil, []map[string]any{{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{
				"name": "issue_set_acceptance",
				"arguments": map[string]any{
					"feature_id":          "f-1",
					"linear_issue_id":     "i-1",
					"acceptance_criteria": txt,
				},
			},
		}})
		if resps[0]["result"].(map[string]any)["isError"] != false {
			t.Fatalf("set %q: %v", txt, resps[0]["result"])
		}
	}
	got, _ := db.GetFeatureIssue("f-1", "i-1")
	if got.AcceptanceCriteria != "v2 criteria (refined)" {
		t.Fatalf("upsert did not overwrite: %q", got.AcceptanceCriteria)
	}
}

func TestIssueSetAcceptance_RejectsUnknownFeature(t *testing.T) {
	// Pre-check beats the raw FK violation — the host agent sees a
	// readable error rather than a SQLite constraint string.
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "issue_set_acceptance",
			"arguments": map[string]any{
				"feature_id":          "no-such",
				"linear_issue_id":     "i-1",
				"acceptance_criteria": "c",
			},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError for unknown feature")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "not found") {
		t.Fatalf("error should say 'not found', got: %s", text)
	}
}

func TestIssueSetAcceptance_RejectsMissingFields(t *testing.T) {
	db := newPlannerTestStore(t)
	cases := []struct {
		name string
		args map[string]any
	}{
		{"no feature", map[string]any{"linear_issue_id": "i", "acceptance_criteria": "c"}},
		{"no issue", map[string]any{"feature_id": "f", "acceptance_criteria": "c"}},
		{"no criteria", map[string]any{"feature_id": "f", "linear_issue_id": "i"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resps := driveServer(t, db, nil, []map[string]any{{
				"jsonrpc": "2.0", "id": 1, "method": "tools/call",
				"params": map[string]any{
					"name": "issue_set_acceptance", "arguments": tc.args,
				},
			}})
			if resps[0]["result"].(map[string]any)["isError"] != true {
				t.Fatal("expected isError")
			}
		})
	}
}

// --- pr_verify_submit ---

// seedForVerify is a one-stop fixture: a feature, an issue under it,
// admiral_tasks claiming the issue with the given PR URL. Returns the
// feature ID so tests can chain into other tools.
func seedForVerify(t *testing.T, db *store.Store, prURL string) string {
	t.Helper()
	const featureID = "f-verify"
	const issueID = "i-verify"
	if err := db.InsertFeatureWithIssues(
		store.Feature{ID: featureID, Name: "v", LinearProjectID: "p-verify", RequirementsText: "r"},
		[]store.FeatureIssue{{LinearIssueID: issueID, AcceptanceCriteria: "criteria"}},
	); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.ClaimAdmiralTask(issueID, "GEO-V", "ev"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	_ = db.UpdateAdmiralTask(issueID, func(at *store.AdmiralTask) {
		at.State = store.JobStateDone
		at.PRURL = prURL
		at.Branch = "linear/GEO-V"
	})
	return featureID
}

func TestPRVerifySubmit_ApproveHappyPath(t *testing.T) {
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/100"
	_ = seedForVerify(t, db, prURL)
	gh := &stubGH{}

	resps := driveServer(t, db, gh, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "pr_verify_submit",
			"arguments": map[string]any{
				"pr_url":    prURL,
				"verdict":   store.PRVerdictApprove,
				"reasoning": "looks good",
				"agent":     "claude",
			},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError true: %v", result["content"])
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload prVerifySubmitResult
	_ = json.Unmarshal([]byte(text), &payload)
	if !payload.Submitted || payload.Verdict != store.PRVerdictApprove {
		t.Fatalf("expected submitted+approve, got %+v", payload)
	}
	// Verify gh was called with the right flag + body.
	if len(gh.postedRevs) != 1 {
		t.Fatalf("want 1 PostReview call, got %d", len(gh.postedRevs))
	}
	if gh.postedRevs[0].verdict != "approve" || gh.postedRevs[0].body != "looks good" {
		t.Fatalf("wrong gh call: %+v", gh.postedRevs[0])
	}
	// Audit row landed.
	v, _ := db.GetLatestPRVerification(prURL)
	if v == nil || v.Verdict != store.PRVerdictApprove || v.Agent != "claude" {
		t.Fatalf("audit row missing: %+v", v)
	}
}

func TestPRVerifySubmit_NeedsRebase_RequestChangesWithRebasePrefix(t *testing.T) {
	// needs_rebase folds into gh's --request-changes but with the
	// rebase-themed prefix so admiral's dispatch_review spawns claude
	// with the right intent.
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/101"
	_ = seedForVerify(t, db, prURL)
	gh := &stubGH{}

	resps := driveServer(t, db, gh, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "pr_verify_submit",
			"arguments": map[string]any{
				"pr_url":    prURL,
				"verdict":   store.PRVerdictNeedsRebase,
				"reasoning": "merge conflict with main",
			},
		},
	}})
	if resps[0]["result"].(map[string]any)["isError"] != false {
		t.Fatalf("unexpected error: %v", resps[0])
	}
	if len(gh.postedRevs) != 1 {
		t.Fatalf("want 1 PostReview, got %d", len(gh.postedRevs))
	}
	got := gh.postedRevs[0]
	if got.verdict != "request_changes" {
		t.Fatalf("needs_rebase should map to request_changes, got %q", got.verdict)
	}
	if !strings.HasPrefix(got.body, "Rebase onto base") {
		t.Fatalf("body should lead with rebase prefix, got: %q", got.body)
	}
	if !strings.Contains(got.body, "merge conflict with main") {
		t.Fatalf("body should retain caller reasoning, got: %q", got.body)
	}
}

func TestPRVerifySubmit_RejectsRequestChangesWithoutReasoning(t *testing.T) {
	// gh enforces --body for --request-changes; surface that to the
	// host agent up front rather than letting gh error out.
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/102"
	_ = seedForVerify(t, db, prURL)

	resps := driveServer(t, db, &stubGH{}, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "pr_verify_submit",
			"arguments": map[string]any{
				"pr_url":  prURL,
				"verdict": store.PRVerdictRequestChanges,
				// no reasoning
			},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "reasoning") {
		t.Fatalf("error should mention reasoning, got: %s", text)
	}
}

func TestPRVerifySubmit_IdempotentSameVerdict(t *testing.T) {
	// Second call with the same verdict must skip the gh call and
	// return submitted=false.
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/103"
	_ = seedForVerify(t, db, prURL)
	gh := &stubGH{}

	call := map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "pr_verify_submit",
			"arguments": map[string]any{
				"pr_url":    prURL,
				"verdict":   store.PRVerdictApprove,
				"reasoning": "first pass ok",
			},
		},
	}
	_ = driveServer(t, db, gh, []map[string]any{call})
	resps := driveServer(t, db, gh, []map[string]any{call})

	text := resps[0]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload prVerifySubmitResult
	_ = json.Unmarshal([]byte(text), &payload)
	if payload.Submitted {
		t.Fatal("second identical call must skip gh")
	}
	if len(gh.postedRevs) != 1 {
		t.Fatalf("expected exactly 1 gh call total, got %d", len(gh.postedRevs))
	}
}

func TestPRVerifySubmit_GHFailure_NoAuditRow(t *testing.T) {
	// PostReview errors -> we must NOT write an audit row, so the next
	// attempt doesn't see "we already submitted" and skip.
	db := newPlannerTestStore(t)
	prURL := "https://github.com/o/r/pull/104"
	_ = seedForVerify(t, db, prURL)
	gh := &stubGH{
		reviewErrs: map[string]error{prURL: errors.New("network down")},
	}

	resps := driveServer(t, db, gh, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "pr_verify_submit",
			"arguments": map[string]any{
				"pr_url":    prURL,
				"verdict":   store.PRVerdictApprove,
				"reasoning": "ok",
			},
		},
	}})
	if resps[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("expected isError on gh failure")
	}
	v, _ := db.GetLatestPRVerification(prURL)
	if v != nil {
		t.Fatalf("audit row must not exist when gh failed, got %+v", v)
	}
}

func TestPRVerifySubmit_NoGHClient(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "pr_verify_submit",
			"arguments": map[string]any{
				"pr_url":  "https://github.com/o/r/pull/1",
				"verdict": store.PRVerdictApprove,
			},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError when gh nil")
	}
	if !strings.Contains(
		result["content"].([]any)[0].(map[string]any)["text"].(string),
		"ADMIRAL_GH_TOKEN",
	) {
		t.Fatal("should mention ADMIRAL_GH_TOKEN")
	}
}

func TestPRVerifySubmit_InvalidVerdict(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, &stubGH{}, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name": "pr_verify_submit",
			"arguments": map[string]any{
				"pr_url":  "https://github.com/o/r/pull/x",
				"verdict": "looks_great", // typo
			},
		},
	}})
	if resps[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("expected isError for unknown verdict")
	}
}

// --- feature_close ---

func TestFeatureClose_HappyPath(t *testing.T) {
	db := newPlannerTestStore(t)
	_ = db.InsertFeature(store.Feature{ID: "f-c", Name: "c", LinearProjectID: "p-c"})

	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "feature_close",
			"arguments": map[string]any{"feature_id": "f-c"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("isError true: %v", result["content"])
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var payload featureCloseResult
	_ = json.Unmarshal([]byte(text), &payload)
	if !payload.Closed {
		t.Fatalf("want closed=true, got %+v", payload)
	}
	f, _ := db.GetFeature("f-c")
	if f.ClosedAt == "" {
		t.Fatal("closed_at not stamped")
	}
}

func TestFeatureClose_AlreadyClosed_IdempotentNoOp(t *testing.T) {
	db := newPlannerTestStore(t)
	_ = db.InsertFeature(store.Feature{ID: "f-c", Name: "c", LinearProjectID: "p-c"})
	_, _ = db.CloseFeature("f-c")

	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "feature_close",
			"arguments": map[string]any{"feature_id": "f-c"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("re-close should be a clean no-op, got: %v", result["content"])
	}
	var payload featureCloseResult
	_ = json.Unmarshal([]byte(result["content"].([]any)[0].(map[string]any)["text"].(string)), &payload)
	if payload.Closed {
		t.Fatal("second close should report closed=false")
	}
	if !strings.Contains(payload.Reason, "already closed") {
		t.Fatalf("reason should explain idempotency, got: %q", payload.Reason)
	}
}

func TestFeatureClose_UnknownFeature(t *testing.T) {
	db := newPlannerTestStore(t)
	resps := driveServer(t, db, nil, []map[string]any{{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "feature_close",
			"arguments": map[string]any{"feature_id": "ghost"},
		},
	}})
	result := resps[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatal("expected isError for unknown feature")
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "not found") {
		t.Fatalf("error should say 'not found', got: %s", text)
	}
}
