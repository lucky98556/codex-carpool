package quota

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

const (
	maxCapturedRequestBodyBytes = 4 << 20
	maxRequestContentRunes      = 2_000
	maxContentFilterScanRunes   = 64_000
	capturedRequestContentTTL   = 2 * time.Minute
	pendingRequestContentTTL    = 2 * time.Hour
	maxCapturedRequestContent   = 4_096
	maxPendingRequestContent    = 4_096
)

type capturedRequestContent struct {
	KeyID      string
	Model      string
	Content    string
	Match      ContentFilterMatch
	CapturedAt time.Time
}

type pendingRequestContent struct {
	Model       string
	Content     string
	RequestedAt time.Time
}

type requestContentEnvelope struct {
	Messages []requestContentMessage `json:"messages"`
	Input    json.RawMessage         `json:"input"`
	Prompt   json.RawMessage         `json:"prompt"`
}

type requestContentMessage struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Text    string          `json:"text"`
	Content json.RawMessage `json:"content"`
}

// CaptureRequestContent is called by the CPA before-auth interceptor. Parsing
// is restricted to enabled managed Keys, keeping unmanaged traffic off this
// plugin's request-content path.
func (engine *Engine) CaptureRequestContent(rawAPIKey, model string, body []byte, now time.Time) string {
	if engine == nil || len(body) == 0 || len(body) > maxCapturedRequestBodyBytes || engine.admissionsClosed.Load() {
		return ""
	}
	engine.adminMu.RLock()
	engine.configMu.RLock()
	secret := engine.config.KeyHMACSecret
	engine.configMu.RUnlock()
	fingerprint := FingerprintAPIKey(rawAPIKey, secret)
	engine.policiesMu.RLock()
	keyID, found := engine.policiesByHash[fingerprint]
	policy := engine.policiesByID[keyID]
	engine.policiesMu.RUnlock()
	engine.adminMu.RUnlock()
	if fingerprint == "" || !found || !policy.Enabled {
		return ""
	}
	scanContent := extractUserRequestContentWithLimit(body, maxContentFilterScanRunes)
	if scanContent == "" {
		return ""
	}
	content := normalizeRequestContent(scanContent)
	match := engine.matchForbiddenContent(scanContent)
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return ""
	}
	captureID := hex.EncodeToString(random[:])
	now = now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	engine.requestContentMu.Lock()
	engine.pruneRequestContentLocked(now)
	if len(engine.capturedRequestContent) < maxCapturedRequestContent {
		engine.capturedRequestContent[captureID] = capturedRequestContent{
			KeyID: keyID, Model: strings.TrimSpace(model), Content: content, Match: match, CapturedAt: now,
		}
	} else {
		captureID = ""
	}
	engine.requestContentMu.Unlock()
	return captureID
}

func (engine *Engine) claimCapturedRequestContent(captureID, keyID string, now time.Time) capturedRequestContent {
	captureID = strings.TrimSpace(captureID)
	if engine == nil || captureID == "" || keyID == "" {
		return capturedRequestContent{}
	}
	engine.requestContentMu.Lock()
	defer engine.requestContentMu.Unlock()
	engine.pruneRequestContentLocked(now.UTC())
	captured, exists := engine.capturedRequestContent[captureID]
	if !exists {
		return capturedRequestContent{}
	}
	delete(engine.capturedRequestContent, captureID)
	if captured.KeyID != keyID {
		return capturedRequestContent{}
	}
	return captured
}

func (engine *Engine) rememberPendingRequestContent(key allocationBucketKey, model, content string, requestedAt time.Time) {
	content = normalizeRequestContent(content)
	if engine == nil || key.KeyID == "" || key.AuthID == "" || content == "" {
		return
	}
	engine.requestContentMu.Lock()
	defer engine.requestContentMu.Unlock()
	engine.pruneRequestContentLocked(requestedAt.UTC())
	if engine.pendingRequestCount >= maxPendingRequestContent {
		return
	}
	engine.pendingRequestContent[key] = append(engine.pendingRequestContent[key], pendingRequestContent{
		Model: strings.TrimSpace(model), Content: content, RequestedAt: requestedAt.UTC(),
	})
	engine.pendingRequestCount++
}

func (engine *Engine) takePendingRequestContent(key allocationBucketKey, model string, requestedAt time.Time) string {
	if engine == nil || key.KeyID == "" || key.AuthID == "" {
		return ""
	}
	engine.requestContentMu.Lock()
	defer engine.requestContentMu.Unlock()
	items := engine.pendingRequestContent[key]
	if len(items) == 0 {
		return ""
	}
	model = strings.TrimSpace(model)
	best := -1
	bestDistance := time.Duration(1<<63 - 1)
	for index, item := range items {
		if model != "" && item.Model != "" && item.Model != model {
			continue
		}
		distance := requestedAt.UTC().Sub(item.RequestedAt)
		if distance < 0 {
			distance = -distance
		}
		if best < 0 || distance < bestDistance {
			best, bestDistance = index, distance
		}
	}
	if best < 0 {
		best = 0
	}
	content := items[best].Content
	items = append(items[:best], items[best+1:]...)
	engine.pendingRequestCount--
	if len(items) == 0 {
		delete(engine.pendingRequestContent, key)
	} else {
		engine.pendingRequestContent[key] = items
	}
	return content
}

func (engine *Engine) discardPendingRequestContentForKey(keyID string) {
	if engine == nil || keyID == "" {
		return
	}
	engine.requestContentMu.Lock()
	defer engine.requestContentMu.Unlock()
	for captureID, item := range engine.capturedRequestContent {
		if item.KeyID == keyID {
			delete(engine.capturedRequestContent, captureID)
		}
	}
	for key, items := range engine.pendingRequestContent {
		if key.KeyID == keyID {
			engine.pendingRequestCount -= len(items)
			delete(engine.pendingRequestContent, key)
		}
	}
	if engine.pendingRequestCount < 0 {
		engine.pendingRequestCount = 0
	}
}

func (engine *Engine) pruneRequestContentLocked(now time.Time) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	capturedCutoff := now.Add(-capturedRequestContentTTL)
	for captureID, item := range engine.capturedRequestContent {
		if item.CapturedAt.Before(capturedCutoff) {
			delete(engine.capturedRequestContent, captureID)
		}
	}
	pendingCutoff := now.Add(-pendingRequestContentTTL)
	for key, items := range engine.pendingRequestContent {
		kept := items[:0]
		for _, item := range items {
			if !item.RequestedAt.Before(pendingCutoff) {
				kept = append(kept, item)
			} else {
				engine.pendingRequestCount--
			}
		}
		if len(kept) == 0 {
			delete(engine.pendingRequestContent, key)
		} else {
			engine.pendingRequestContent[key] = kept
		}
	}
	if engine.pendingRequestCount < 0 {
		engine.pendingRequestCount = 0
	}
}

func extractUserRequestContent(body []byte) string {
	return extractUserRequestContentWithLimit(body, maxRequestContentRunes)
}

func extractUserRequestContentWithLimit(body []byte, limit int) string {
	var envelope requestContentEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	for index := len(envelope.Messages) - 1; index >= 0; index-- {
		if strings.EqualFold(strings.TrimSpace(envelope.Messages[index].Role), "user") {
			if content := requestMessageTextWithLimit(envelope.Messages[index], limit); content != "" {
				return content
			}
		}
	}
	if content := requestInputTextWithLimit(envelope.Input, limit); content != "" {
		return content
	}
	return requestScalarTextWithLimit(envelope.Prompt, limit)
}

func requestInputText(raw json.RawMessage) string {
	return requestInputTextWithLimit(raw, maxRequestContentRunes)
}

func requestInputTextWithLimit(raw json.RawMessage, limit int) string {
	if content := requestScalarTextWithLimit(raw, limit); content != "" {
		return content
	}
	var messages []requestContentMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return ""
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		role := strings.TrimSpace(message.Role)
		if role != "" && !strings.EqualFold(role, "user") {
			continue
		}
		if content := requestMessageTextWithLimit(message, limit); content != "" {
			return content
		}
	}
	return ""
}

func requestMessageText(message requestContentMessage) string {
	return requestMessageTextWithLimit(message, maxRequestContentRunes)
}

func requestMessageTextWithLimit(message requestContentMessage, limit int) string {
	if content := requestScalarTextWithLimit(message.Content, limit); content != "" {
		return content
	}
	if message.Text != "" && (message.Type == "" || message.Type == "text" || message.Type == "input_text") {
		return normalizeRequestContentWithLimit(message.Text, limit)
	}
	var parts []requestContentMessage
	if err := json.Unmarshal(message.Content, &parts); err != nil {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type != "" && part.Type != "text" && part.Type != "input_text" {
			continue
		}
		if text := normalizeRequestContentWithLimit(part.Text, limit); text != "" {
			texts = append(texts, text)
		}
	}
	return normalizeRequestContentWithLimit(strings.Join(texts, "\n"), limit)
}

func requestScalarText(raw json.RawMessage) string {
	return requestScalarTextWithLimit(raw, maxRequestContentRunes)
}

func requestScalarTextWithLimit(raw json.RawMessage, limit int) string {
	if len(raw) == 0 {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return normalizeRequestContentWithLimit(value, limit)
}

func normalizeRequestContent(value string) string {
	return normalizeRequestContentWithLimit(value, maxRequestContentRunes)
}

func normalizeRequestContentWithLimit(value string, limit int) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	if value == "" {
		return ""
	}
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		value = string(runes[:limit]) + "…"
	}
	return value
}
