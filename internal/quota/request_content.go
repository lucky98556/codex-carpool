package quota

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"strings"
	"time"
)

const (
	maxJSONRequestContentBytes = 4 << 20
	maxRequestContentRunes     = 2_000
	maxContentFilterScanRunes  = 64_000
	capturedRequestContentTTL  = 2 * time.Minute
	maxCapturedRequestContent  = 4_096
)

type capturedRequestContent struct {
	KeyID      string
	Model      string
	Content    string
	Match      ContentFilterMatch
	CapturedAt time.Time
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
// is restricted to registered Keys. A disabled policy bypasses enforcement,
// but its terminal request audit still retains the same bounded user excerpt.
func (engine *Engine) CaptureRequestContent(rawAPIKey, model, contentType string, body []byte, now time.Time) string {
	if engine == nil || len(body) == 0 || engine.admissionsClosed.Load() {
		return ""
	}
	engine.adminMu.RLock()
	engine.configMu.RLock()
	secret := engine.config.KeyHMACSecret
	engine.configMu.RUnlock()
	fingerprint := FingerprintAPIKey(rawAPIKey, secret)
	engine.policiesMu.RLock()
	keyID, found := engine.policiesByHash[fingerprint]
	engine.policiesMu.RUnlock()
	engine.adminMu.RUnlock()
	if fingerprint == "" || !found {
		return ""
	}
	scanContent := extractUserRequestContentWithTypeAndLimit(body, contentType, maxContentFilterScanRunes)
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

func (engine *Engine) discardCapturedRequestContentForKey(keyID string) {
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
}

func extractUserRequestContent(body []byte) string {
	return extractUserRequestContentWithTypeAndLimit(body, "application/json", maxRequestContentRunes)
}

func extractUserRequestContentWithLimit(body []byte, limit int) string {
	return extractUserRequestContentWithTypeAndLimit(body, "application/json", limit)
}

func extractUserRequestContentWithType(body []byte, contentType string) string {
	return extractUserRequestContentWithTypeAndLimit(body, contentType, maxRequestContentRunes)
}

func extractUserRequestContentWithTypeAndLimit(body []byte, contentType string, limit int) string {
	mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err == nil && strings.EqualFold(mediaType, "multipart/form-data") {
		return requestMultipartPromptWithLimit(body, params["boundary"], limit)
	}
	if len(body) > maxJSONRequestContentBytes {
		// Large JSON image edits commonly carry a top-level base64 image. Decode
		// only prompt so the image field is validated but never retained.
		var promptOnly struct {
			Prompt json.RawMessage `json:"prompt"`
		}
		if err := json.Unmarshal(body, &promptOnly); err != nil {
			return ""
		}
		return requestScalarTextWithLimit(promptOnly.Prompt, limit)
	}
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

// Image-edit requests can contain large binary file parts. Read only the
// prompt field from the existing body and never copy uploaded image content
// into the captured request excerpt.
func requestMultipartPromptWithLimit(body []byte, boundary string, limit int) string {
	boundary = strings.TrimSpace(boundary)
	if boundary == "" {
		return ""
	}
	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return ""
		}
		if err != nil {
			return ""
		}
		if part.FormName() != "prompt" || part.FileName() != "" {
			_ = part.Close()
			continue
		}
		value, err := io.ReadAll(io.LimitReader(part, maxJSONRequestContentBytes+1))
		_ = part.Close()
		if err != nil || len(value) > maxJSONRequestContentBytes {
			return ""
		}
		return normalizeRequestContentWithLimit(string(value), limit)
	}
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
