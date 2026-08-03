package quota

import (
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExtractUserRequestContentKeepsOnlyLatestUserText(t *testing.T) {
	body := []byte(`{
		"messages": [
			{"role": "system", "content": "never persist this system prompt"},
			{"role": "user", "content": "第一条用户消息"},
			{"role": "assistant", "content": "never persist this response"},
			{"role": "user", "content": [
				{"type": "input_text", "text": "你好"},
				{"type": "input_image", "image_url": "data:image/png;base64,secret"},
				{"type": "text", "text": "请帮我分析"}
			]}
		]
	}`)
	if got := extractUserRequestContent(body); got != "你好\n请帮我分析" {
		t.Fatalf("extractUserRequestContent() = %q", got)
	}
}

func TestExtractUserRequestContentSupportsResponsesInputAndPrompt(t *testing.T) {
	responsesBody := []byte(`{
		"input": [
			{"role": "developer", "content": "never persist this instruction"},
			{"role": "user", "content": [{"type": "input_text", "text": "用户输入"}]},
			{"type": "tool_call", "text": "never persist this tool call"}
		]
	}`)
	if got := extractUserRequestContent(responsesBody); got != "用户输入" {
		t.Fatalf("Responses input = %q", got)
	}
	if got := extractUserRequestContent([]byte(`{"prompt":"你好"}`)); got != "你好" {
		t.Fatalf("prompt = %q", got)
	}
	if got := extractUserRequestContent([]byte(`{"messages":[{"role":"tool","content":"secret"}]}`)); got != "" {
		t.Fatalf("tool-only request = %q, want empty", got)
	}
}

func TestExtractUserRequestContentIsBoundedByUnicodeRunes(t *testing.T) {
	content := strings.Repeat("你", maxRequestContentRunes+50)
	got := extractUserRequestContent([]byte(`{"prompt":"` + content + `"}`))
	if runes := []rune(got); len(runes) != maxRequestContentRunes+1 || runes[len(runes)-1] != '…' {
		t.Fatalf("bounded content length = %d, suffix = %q", len(runes), string(runes[len(runes)-1:]))
	}
}

func TestCapturedRequestContentFollowsItsTerminalUsageLog(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", AllocationX: 1, Enabled: true})
	defer func() { _ = engine.Close() }()

	now := time.Now().UTC().Truncate(time.Second)
	candidates := readyNativeCandidate(t, engine, now)
	captureID := engine.CaptureRequestContent(
		"managed-key",
		"gpt-5",
		[]byte(`{"messages":[{"role":"user","content":"你好"}]}`),
		now,
	)
	if captureID == "" {
		t.Fatal("CaptureRequestContent() did not capture a managed Key request")
	}
	admission := engine.AdmitCaptured("managed-key", "gpt-5", captureID, now, candidates)
	if !admission.Allowed || admission.AuthID != "account-a" {
		t.Fatalf("AdmitCaptured() = %+v", admission)
	}
	engine.RecordUsage(CompletedUsage{
		APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5",
		RequestedAt: now, Generate: true, TotalTokens: 123,
	})
	if err := engine.flushPending(); err != nil {
		t.Fatalf("flushPending() = %v", err)
	}
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil {
		t.Fatalf("DecisionLogs() = %v", err)
	}
	if len(logs) != 1 || logs[0].Decision != "completed" || logs[0].RequestContent != "你好" || logs[0].AuthID != "account-a" {
		t.Fatalf("terminal request log = %+v", logs)
	}
}

func TestRequestContentColumnMigratesWithoutLosingOldLogs(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open legacy sqlite: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE request_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		key_id TEXT NOT NULL,
		auth_id TEXT NOT NULL,
		model TEXT NOT NULL,
		requested_at INTEGER NOT NULL,
		decision TEXT NOT NULL,
		status_code INTEGER NOT NULL,
		reason TEXT NOT NULL,
		units INTEGER NOT NULL
	);
	INSERT INTO request_logs(key_id, auth_id, model, requested_at, decision, status_code, reason, units)
	VALUES ('managed', 'account-full-id', 'gpt-5', 1, 'completed', 200, 'legacy', 12);`); err != nil {
		_ = db.Close()
		t.Fatalf("create legacy request log: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy sqlite: %v", err)
	}

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("OpenStore() migration: %v", err)
	}
	defer func() { _ = store.Close() }()
	if exists, err := store.columnExists("request_logs", "request_content"); err != nil || !exists {
		t.Fatalf("request_content column exists = %v, err=%v", exists, err)
	}
	logs, total, err := store.ListDecisionLogsPage("managed", "", "", 10, 0)
	if err != nil || total != 1 || len(logs) != 1 || logs[0].AuthID != "account-full-id" || logs[0].RequestContent != "" {
		t.Fatalf("migrated legacy logs = %+v, total=%d, err=%v", logs, total, err)
	}
}
