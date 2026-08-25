package quota

import (
	"bytes"
	"mime/multipart"
	"strings"
	"testing"
	"time"
)

func TestExtractUserRequestContentKeepsOnlyLatestUserText(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"secret"},{"role":"user","content":"第一条"},{"role":"assistant","content":"hidden"},{"role":"user","content":[{"type":"input_text","text":"你好"},{"type":"text","text":"请分析"}]}]}`)
	if got := extractUserRequestContent(body); got != "你好\n请分析" {
		t.Fatalf("extractUserRequestContent() = %q", got)
	}
}

func TestExtractUserRequestContentIsBoundedByUnicodeRunes(t *testing.T) {
	content := strings.Repeat("你", maxRequestContentRunes+50)
	got := extractUserRequestContent([]byte(`{"prompt":"` + content + `"}`))
	if runes := []rune(got); len(runes) != maxRequestContentRunes+1 || runes[len(runes)-1] != '…' {
		t.Fatalf("bounded content length = %d", len(runes))
	}
}

func TestExtractUserRequestContentReadsLargeMultipartImageEditPrompt(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", "gpt-image-2"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("prompt", "把沙发改成暖灰色，保留房间布局"); err != nil {
		t.Fatal(err)
	}
	image, err := writer.CreateFormFile("image", "room.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = image.Write(bytes.Repeat([]byte{0xab}, maxJSONRequestContentBytes+1024)); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if body.Len() <= maxJSONRequestContentBytes {
		t.Fatalf("multipart body = %d bytes, want larger than JSON capture limit", body.Len())
	}
	if got := extractUserRequestContentWithType(body.Bytes(), writer.FormDataContentType()); got != "把沙发改成暖灰色，保留房间布局" {
		t.Fatalf("multipart image-edit content = %q", got)
	}
}

func TestExtractUserRequestContentReadsLargeJSONImageEditPrompt(t *testing.T) {
	body := []byte(`{"model":"gpt-image-2","image":"data:image/png;base64,` + strings.Repeat("A", maxJSONRequestContentBytes+1024) + `","prompt":"把背景改成纯白色"}`)
	if len(body) <= maxJSONRequestContentBytes {
		t.Fatalf("JSON body = %d bytes, want larger than normal capture limit", len(body))
	}
	if got := extractUserRequestContentWithType(body, "application/json; charset=utf-8"); got != "把背景改成纯白色" {
		t.Fatalf("large JSON image-edit content = %q", got)
	}
}

func TestMultipartImageEditPromptFollowsTerminalUsageLog(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true, AllowedModels: []string{"gpt-image-2"}})
	defer func() { _ = engine.Close() }()
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-image-2"}}); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("prompt", "移除图片左侧的纸箱"); err != nil {
		t.Fatal(err)
	}
	image, err := writer.CreateFormFile("image", "source.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = image.Write([]byte("binary image content must not enter the log")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	captureID := engine.CaptureRequestContent("managed-key", "gpt-image-2", writer.FormDataContentType(), body.Bytes(), now)
	admission := engine.AdmitCaptured("managed-key", "gpt-image-2", captureID, now, readyNativeCandidate(t, engine, now))
	if !admission.Allowed {
		t.Fatalf("AdmitCaptured() = %+v", admission)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: admission.AuthID, Model: "gpt-image-2", RequestedAt: now, Generate: true, InputTokens: 847, OutputTokens: 172, TotalTokens: 1019})
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil || len(logs) != 1 || logs[0].Decision != "completed" || logs[0].RequestContent != "移除图片左侧的纸箱" || logs[0].InputTokens != 847 || logs[0].OutputTokens != 172 {
		t.Fatalf("multipart image-edit request log = %+v, err=%v", logs, err)
	}
}

func TestCapturedRequestContentFollowsTerminalUsageLog(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: true})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Second)
	captureID := engine.CaptureRequestContent("managed-key", "gpt-5", "application/json", []byte(`{"prompt":"你好"}`), now)
	admission := engine.AdmitCaptured("managed-key", "gpt-5", captureID, now, readyNativeCandidate(t, engine, now))
	if !admission.Allowed {
		t.Fatalf("AdmitCaptured() = %+v", admission)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, TotalTokens: 123})
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil || len(logs) != 1 || logs[0].RequestContent != "你好" {
		t.Fatalf("terminal request log = %+v, err=%v", logs, err)
	}
}

func TestDisabledRegisteredKeyKeepsFullAccountingWithoutBudgetRejection(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: false, FiveHourBudgetUSD: 0.5, SevenDayBudgetUSD: 0.5, AllowedModels: []string{"gpt-5"}})
	defer func() { _ = engine.Close() }()
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := engine.ReplaceModelRates([]ModelRate{{Model: "gpt-5", InputUSDPerMillion: 10, OutputUSDPerMillion: 20}}); err != nil {
		t.Fatal(err)
	}
	captureID := engine.CaptureRequestContent("managed-key", "gpt-5", "application/json", []byte(`{"prompt":"暂停也记录"}`), now)
	if admission := engine.AdmitCaptured("managed-key", "gpt-5", captureID, now); !admission.Bypass {
		t.Fatalf("disabled admission = %+v", admission)
	}
	engine.RecordUsage(CompletedUsage{APIKey: "managed-key", AuthID: "account-a", Model: "gpt-5", RequestedAt: now, Generate: true, InputTokens: 100_000, OutputTokens: 20_000, TotalTokens: 120_000})
	logs, err := engine.DecisionLogs("managed", 10)
	if err != nil || len(logs) != 1 || logs[0].RequestContent != "暂停也记录" || logs[0].CostMicros != 1_400_000 {
		t.Fatalf("disabled audit = %+v, err=%v", logs, err)
	}
	if spend := engine.Summary(now).Keys[0].DollarSpend; spend.FiveHour.SpentUSD != 1.4 || spend.SevenDay.SpentUSD != 1.4 || spend.FiveHour.CoolingUntil == nil || spend.SevenDay.CoolingUntil == nil {
		t.Fatalf("disabled Key dollar windows = %+v, want full $1.40 accounting", spend)
	}
	records, err := engine.UsageRecords("managed", 10)
	if err != nil || len(records) != 1 || records[0].Units != 120_000 || records[0].InputTokens != 100_000 || records[0].OutputTokens != 20_000 {
		t.Fatalf("disabled Key usage records = %+v, err=%v", records, err)
	}
	analysis, err := engine.UsageAnalysis("managed", now.Add(-time.Hour), now.Add(time.Hour), time.UTC, "hour")
	if err != nil || analysis.TotalTokens != 120_000 || analysis.RequestCount != 1 || analysis.InputTokens != 100_000 || analysis.OutputTokens != 20_000 || analysis.CostMicros != 1_400_000 {
		t.Fatalf("disabled Key usage analysis = %+v, err=%v", analysis, err)
	}
	if err := engine.flushPending(); err != nil {
		t.Fatalf("flush disabled Key accounting = %v", err)
	}
	summary, err := engine.SummaryWithActualTokens(now.Add(time.Second))
	if err != nil || len(summary.Keys) != 1 || summary.Keys[0].ActualTokens.Total != 120_000 || summary.Keys[0].ActualTokens.Input != 100_000 || summary.Keys[0].ActualTokens.Output != 20_000 {
		t.Fatalf("disabled Key actual Token totals = %+v, err=%v", summary.Keys, err)
	}
	if admission := engine.Admit("managed-key", "gpt-5", now.Add(time.Second)); !admission.Bypass || admission.Code != "" {
		t.Fatalf("disabled Key after budget exhaustion = %+v, want unblocked CPA routing", admission)
	}
	if admission := engine.Admit("managed-key", "gpt-4", now.Add(2*time.Second)); admission.Bypass || admission.Code != "model_not_allowed" {
		t.Fatalf("disabled Key model restriction = %+v, want model_not_allowed", admission)
	}
}

func TestDisabledRegisteredKeyStillRequiresConfiguredModelRate(t *testing.T) {
	engine := newTestEngine(t, KeyPolicy{ID: "managed", Name: "Managed", Enabled: false})
	defer func() { _ = engine.Close() }()
	if admission := engine.Admit("managed-key", "unpriced-model", time.Now().UTC()); admission.Bypass || admission.Code != "model_rate_not_configured" {
		t.Fatalf("disabled Key missing-rate gate = %+v, want model_rate_not_configured", admission)
	}
}
