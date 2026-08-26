package quota

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRetentionKeepsRecentLogsAndWritesCleanupOperationalLog(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()

	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-31 * 24 * time.Hour)
	recent := now.Add(-20 * 24 * time.Hour)
	if err := engine.store.FlushUsageAndLogs(nil, []DecisionLog{
		{KeyID: "managed", RequestedAt: old, Decision: "completed", Reason: "completed", RequestContent: "old usage"},
		{KeyID: "managed", RequestedAt: old, Decision: "blocked", Reason: "content_forbidden", RequestContent: "old forbidden"},
		{KeyID: "managed", RequestedAt: recent, Decision: "completed", Reason: "completed", RequestContent: "recent usage"},
	}); err != nil {
		t.Fatalf("seed request logs: %v", err)
	}
	if err := engine.store.AppendOperationalLog(OperationalLog{OccurredAt: old, Level: "info", Event: "old_event", Message: "old runtime log"}); err != nil {
		t.Fatalf("seed operational log: %v", err)
	}

	engine.pruneRetention(now)

	logs, total, err := engine.store.ListDecisionLogsPage("", "", "", 20, 0)
	if err != nil {
		t.Fatalf("list retained request logs: %v", err)
	}
	if total != 1 || len(logs) != 1 || logs[0].RequestContent != "recent usage" {
		t.Fatalf("retained request logs = %+v, total=%d", logs, total)
	}
	operations, _, err := engine.store.ListOperationalLogsPage("", "", 20, 0)
	if err != nil {
		t.Fatalf("list retained operational logs: %v", err)
	}
	if len(operations) != 1 || operations[0].Event != "log_retention_cleanup" || !strings.Contains(operations[0].Message, "使用日志 1 条，内容拦截日志 1 条，运行日志 1 条") {
		t.Fatalf("cleanup operational logs = %+v", operations)
	}
	storage, err := engine.LogStorage()
	if err != nil {
		t.Fatalf("LogStorage(): %v", err)
	}
	if storage.DatabaseBytes <= 0 || storage.UsageRows != 1 || storage.ForbiddenRows != 0 || storage.OperationalRows != 1 || storage.RetentionDays != 30 {
		t.Fatalf("log storage = %+v", storage)
	}
}

func TestUsageAndForbiddenLogViewsStayIndependent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native plugin database lock is Linux-only")
	}
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC()
	if err := engine.store.FlushUsageAndLogs(nil, []DecisionLog{
		{KeyID: "managed", RequestedAt: now, Decision: "completed", Reason: "completed", RequestContent: "usage"},
		{KeyID: "managed", RequestedAt: now.Add(time.Second), Decision: "blocked", Reason: "content_forbidden", RequestContent: "forbidden"},
	}); err != nil {
		t.Fatal(err)
	}
	usage, err := engine.DecisionLogPage("", "", "", 1, 10)
	if err != nil || usage.Total != 1 || len(usage.Logs) != 1 || usage.Logs[0].RequestContent != "usage" {
		t.Fatalf("usage view = %+v, err=%v", usage, err)
	}
	forbidden, err := engine.DecisionLogPage("", "forbidden", "", 1, 10)
	if err != nil || forbidden.Total != 1 || len(forbidden.Logs) != 1 || forbidden.Logs[0].RequestContent != "forbidden" {
		t.Fatalf("forbidden view = %+v, err=%v", forbidden, err)
	}
	storage, err := engine.LogStorage()
	if err != nil || storage.UsageRows != 1 || storage.ForbiddenRows != 1 {
		t.Fatalf("independent storage = %+v, err=%v", storage, err)
	}
	if err := engine.ClearDecisionLogs(""); err != nil {
		t.Fatal(err)
	}
	forbidden, err = engine.DecisionLogPage("", "forbidden", "", 1, 10)
	if err != nil || forbidden.Total != 1 {
		t.Fatalf("clearing usage logs removed forbidden logs: %+v, err=%v", forbidden, err)
	}
}
