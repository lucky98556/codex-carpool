package quota

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	contentFilterSourceBuiltin    = "builtin"
	contentFilterSourceCustom     = "custom"
	maxContentFilterTerms         = 1_000
	maxContentFilterTermRunes     = 128
	maxContentFilterIDRunes       = 96
	maxContentFilterCategoryRunes = 64
)

// ContentFilterTerm is one literal, case-insensitive phrase. Regex syntax is
// intentionally unsupported so operators can reason about every match.
type ContentFilterTerm struct {
	ID       string `json:"id"`
	Value    string `json:"value"`
	Category string `json:"category"`
	Source   string `json:"source"`
	Enabled  bool   `json:"enabled"`
}

// ContentFilterSettings controls the optional pre-routing content guard.
// Disabled is the upgrade-safe default for existing installations.
type ContentFilterSettings struct {
	Enabled bool                `json:"enabled"`
	Terms   []ContentFilterTerm `json:"terms"`
}

// ContentFilterMatch contains only the matched phrase metadata. The bounded
// request excerpt itself remains in the existing request-log column.
type ContentFilterMatch struct {
	Matched  bool   `json:"matched"`
	Term     string `json:"term,omitempty"`
	Category string `json:"category,omitempty"`
}

var builtinContentFilterTerms = []ContentFilterTerm{
	{ID: "builtin-bomb-zh", Value: "制作炸弹教程", Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-explosive-zh", Value: "自制爆炸物教程", Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-firearms-zh", Value: "买卖枪支", Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-drugs-zh", Value: "制造冰毒", Category: "drugs", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-csam-zh", Value: "儿童色情资源", Category: "sexual_minors", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-card-fraud-zh", Value: "盗刷信用卡教程", Category: "fraud", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-ransomware-zh", Value: "勒索软件源码", Category: "malware", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-ddos-zh", Value: "DDoS代打", Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-hitman-zh", Value: "雇凶杀人", Category: "violence", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-bomb-en", Value: "how to build a bomb", Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-firearms-en", Value: "buy illegal firearms", Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-csam-en", Value: "child sexual abuse material", Category: "sexual_minors", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-ransomware-en", Value: "ransomware source code", Category: "malware", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-ddos-en", Value: "ddos for hire", Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-hitman-en", Value: "hire a hitman", Category: "violence", Source: contentFilterSourceBuiltin, Enabled: true},
}

type contentFilterOutput struct {
	term     string
	category string
}

type contentFilterNode struct {
	next map[rune]int
	fail int
	out  []contentFilterOutput
}

type contentFilterMatcher struct {
	nodes []contentFilterNode
}

func compileContentFilterMatcher(terms []ContentFilterTerm) *contentFilterMatcher {
	matcher := &contentFilterMatcher{nodes: []contentFilterNode{{next: make(map[rune]int)}}}
	for _, term := range terms {
		if !term.Enabled {
			continue
		}
		value := normalizeContentFilterText(term.Value)
		if value == "" {
			continue
		}
		state := 0
		for _, char := range value {
			next, ok := matcher.nodes[state].next[char]
			if !ok {
				next = len(matcher.nodes)
				matcher.nodes[state].next[char] = next
				matcher.nodes = append(matcher.nodes, contentFilterNode{next: make(map[rune]int)})
			}
			state = next
		}
		matcher.nodes[state].out = append(matcher.nodes[state].out, contentFilterOutput{term: term.Value, category: term.Category})
	}
	queue := make([]int, 0, len(matcher.nodes))
	for _, child := range matcher.nodes[0].next {
		queue = append(queue, child)
	}
	for len(queue) > 0 {
		state := queue[0]
		queue = queue[1:]
		for char, child := range matcher.nodes[state].next {
			queue = append(queue, child)
			fallback := matcher.nodes[state].fail
			for fallback != 0 {
				if target, ok := matcher.nodes[fallback].next[char]; ok {
					fallback = target
					break
				}
				fallback = matcher.nodes[fallback].fail
			}
			if fallback == 0 {
				if target, ok := matcher.nodes[0].next[char]; ok && target != child {
					fallback = target
				}
			}
			matcher.nodes[child].fail = fallback
			matcher.nodes[child].out = append(matcher.nodes[child].out, matcher.nodes[fallback].out...)
		}
	}
	return matcher
}

func (matcher *contentFilterMatcher) Match(value string) ContentFilterMatch {
	if matcher == nil || len(matcher.nodes) <= 1 {
		return ContentFilterMatch{}
	}
	state := 0
	for _, char := range normalizeContentFilterText(value) {
		for state != 0 {
			if _, ok := matcher.nodes[state].next[char]; ok {
				break
			}
			state = matcher.nodes[state].fail
		}
		if next, ok := matcher.nodes[state].next[char]; ok {
			state = next
		}
		if len(matcher.nodes[state].out) > 0 {
			match := matcher.nodes[state].out[0]
			return ContentFilterMatch{Matched: true, Term: match.term, Category: match.category}
		}
	}
	return ContentFilterMatch{}
}

func normalizeContentFilterText(value string) string {
	return strings.Map(func(char rune) rune {
		if unicode.IsSpace(char) {
			return ' '
		}
		return unicode.ToLower(char)
	}, strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func normalizeContentFilterSettings(settings ContentFilterSettings) (ContentFilterSettings, error) {
	builtinByID := make(map[string]ContentFilterTerm, len(builtinContentFilterTerms))
	for _, term := range builtinContentFilterTerms {
		builtinByID[term.ID] = term
	}
	result := ContentFilterSettings{Enabled: settings.Enabled, Terms: make([]ContentFilterTerm, 0, len(settings.Terms)+len(builtinByID))}
	seenIDs, seenValues := make(map[string]struct{}), make(map[string]struct{})
	for _, term := range settings.Terms {
		term.ID = strings.TrimSpace(term.ID)
		if builtin, ok := builtinByID[term.ID]; ok {
			builtin.Enabled = term.Enabled
			term = builtin
		} else {
			term.Value = strings.TrimSpace(term.Value)
			term.Category = strings.TrimSpace(term.Category)
			term.Source = contentFilterSourceCustom
			if term.ID == "" {
				term.ID = fmt.Sprintf("custom-%d", time.Now().UTC().UnixNano()+int64(len(result.Terms)))
			}
			if !strings.HasPrefix(term.ID, "custom-") {
				return ContentFilterSettings{}, fmt.Errorf("custom content-filter term id must start with custom-")
			}
		}
		if term.Value == "" || utf8.RuneCountInString(term.Value) > maxContentFilterTermRunes {
			return ContentFilterSettings{}, fmt.Errorf("content-filter terms must contain 1 to %d characters", maxContentFilterTermRunes)
		}
		if utf8.RuneCountInString(term.ID) > maxContentFilterIDRunes {
			return ContentFilterSettings{}, fmt.Errorf("content-filter term ids support at most %d characters", maxContentFilterIDRunes)
		}
		if term.Category == "" {
			term.Category = "custom"
		}
		if utf8.RuneCountInString(term.Category) > maxContentFilterCategoryRunes {
			return ContentFilterSettings{}, fmt.Errorf("content-filter categories support at most %d characters", maxContentFilterCategoryRunes)
		}
		normalized := normalizeContentFilterText(term.Value)
		if _, exists := seenIDs[term.ID]; exists {
			return ContentFilterSettings{}, fmt.Errorf("duplicate content-filter term id %q", term.ID)
		}
		if _, exists := seenValues[normalized]; exists {
			return ContentFilterSettings{}, fmt.Errorf("duplicate content-filter term %q", term.Value)
		}
		seenIDs[term.ID] = struct{}{}
		seenValues[normalized] = struct{}{}
		result.Terms = append(result.Terms, term)
		delete(builtinByID, term.ID)
	}
	for _, builtin := range builtinContentFilterTerms {
		if _, missing := builtinByID[builtin.ID]; missing {
			result.Terms = append(result.Terms, builtin)
		}
	}
	if len(result.Terms) > maxContentFilterTerms {
		return ContentFilterSettings{}, fmt.Errorf("content-filter supports at most %d terms", maxContentFilterTerms)
	}
	sort.SliceStable(result.Terms, func(left, right int) bool {
		if result.Terms[left].Source != result.Terms[right].Source {
			return result.Terms[left].Source == contentFilterSourceBuiltin
		}
		return result.Terms[left].Value < result.Terms[right].Value
	})
	return result, nil
}

func (engine *Engine) ContentFilterSettings() ContentFilterSettings {
	if engine == nil {
		return ContentFilterSettings{}
	}
	engine.contentFilterMu.RLock()
	defer engine.contentFilterMu.RUnlock()
	settings := engine.contentFilterSettings
	settings.Terms = append([]ContentFilterTerm(nil), settings.Terms...)
	return settings
}

func (engine *Engine) ConfigureContentFilter(settings ContentFilterSettings) (ContentFilterSettings, error) {
	if engine == nil {
		return ContentFilterSettings{}, fmt.Errorf("codex-carpool is not initialized")
	}
	validated, err := normalizeContentFilterSettings(settings)
	if err != nil {
		return ContentFilterSettings{}, err
	}
	if err := engine.store.SaveContentFilterSettings(validated); err != nil {
		return ContentFilterSettings{}, err
	}
	engine.contentFilterMu.Lock()
	engine.contentFilterSettings = validated
	engine.contentFilterMatcher = compileContentFilterMatcher(validated.Terms)
	engine.contentFilterMu.Unlock()
	return engine.ContentFilterSettings(), nil
}

func (engine *Engine) matchForbiddenContent(value string) ContentFilterMatch {
	engine.contentFilterMu.RLock()
	defer engine.contentFilterMu.RUnlock()
	if !engine.contentFilterSettings.Enabled {
		return ContentFilterMatch{}
	}
	return engine.contentFilterMatcher.Match(value)
}
