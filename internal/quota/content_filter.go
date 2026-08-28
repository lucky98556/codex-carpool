package quota

import (
	"fmt"
	"regexp"
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
	maxContentFilterTermRunes     = 512
	maxContentFilterIDRunes       = 96
	maxContentFilterCategoryRunes = 64
)

// ContentFilterTerm is one case-insensitive RE2 expression. Go's regexp engine
// provides linear-time matching and deliberately excludes backtracking syntax.
type ContentFilterTerm struct {
	ID       string `json:"id"`
	Value    string `json:"value"`
	Category string `json:"category"`
	Source   string `json:"source"`
	Enabled  bool   `json:"enabled"`
}

// ContentFilterSettings controls the pre-routing content guard. Fresh stores
// enable it by default; a saved operator choice is never overwritten.
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

var builtinContentFilterTerms = append([]ContentFilterTerm{
	{ID: "builtin-regex-v1-explosive-instructions-zh", Value: `(如何|怎么|教程|步骤|方法|配方).{0,16}(制作|制造|组装|自制).{0,8}(炸弹|爆炸物|爆炸装置)`, Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-explosive-reverse-zh", Value: `(炸弹|爆炸物|爆炸装置).{0,8}(制作|制造|组装|自制).{0,8}(教程|步骤|方法|配方)`, Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-firearm-trafficking-zh", Value: `(购买|买到|出售|贩卖|交易).{0,10}(非法|黑市|无证).{0,6}(枪支|枪械|武器)`, Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-firearm-channel-zh", Value: `(非法|黑市|无证).{0,6}(枪支|枪械|武器).{0,10}(购买|出售|交易|渠道|卖家)`, Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-drug-manufacturing-zh", Value: `(如何|怎么|教程|步骤|方法|配方).{0,16}(制造|提纯|合成|生产).{0,8}(冰毒|甲基苯丙胺|海洛因|芬太尼|可卡因)`, Category: "drugs", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-drug-reverse-zh", Value: `(冰毒|甲基苯丙胺|海洛因|芬太尼|可卡因).{0,10}(制造|提纯|合成|生产).{0,8}(教程|步骤|方法|配方)`, Category: "drugs", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-csam-request-zh", Value: `(下载|分享|发送|提供|给我).{0,10}(儿童色情|未成年人色情|儿童性虐待|未成年人性剥削).{0,10}(资源|视频|图片|链接|网站|内容)`, Category: "sexual_minors", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-csam-trade-zh", Value: `(购买|出售|交易).{0,10}(儿童色情|未成年人色情|儿童性虐待|未成年人性剥削).{0,10}(资源|视频|图片|内容)`, Category: "sexual_minors", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-card-fraud-zh", Value: `(写|生成|制作|提供|给我).{0,8}(盗刷信用卡脚本|信用卡盗刷工具|银行卡盗刷代码|信用卡套现教程)`, Category: "fraud", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-phishing-kit-zh", Value: `(写|生成|制作|搭建|提供|给我).{0,8}(钓鱼网站源码|钓鱼页面模板|仿冒登录页代码|钓鱼套件)`, Category: "fraud", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-credential-theft-zh", Value: `(写|生成|制作|提供|给我).{0,8}(窃取账号密码的?脚本|盗取登录凭证的?工具|偷取cookie的?代码|窃取会话令牌的?程序)`, Category: "fraud", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-malware-code-zh", Value: `(写|生成|制作|提供|给我).{0,8}(勒索软件源码|勒索病毒代码|木马控制程序|键盘记录器代码|信息窃取器源码|密码窃取器代码)`, Category: "malware", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-ddos-service-zh", Value: `(ddos|拒绝服务).{0,8}(代打|出租|租用|攻击服务|付费攻击)`, Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-ddos-reverse-zh", Value: `(代打|出租|租用).{0,8}(ddos|拒绝服务攻击)`, Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-botnet-zh", Value: `(搭建|控制|租用|出售).{0,8}(僵尸网络|肉鸡网络).{0,8}(攻击|代打|服务)`, Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-unauthorized-intrusion-zh", Value: `(黑进|攻破|入侵|接管).{0,10}(别人|他人|未授权|未经授权).{0,8}(账号|服务器|网站|邮箱|手机)`, Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-hitman-zh", Value: `(雇凶|找杀手|雇佣杀手).{0,8}(杀人|谋杀|干掉|暗杀)`, Category: "violence", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-poison-zh", Value: `(制作|配制|合成).{0,8}(毒药|毒剂).{0,8}(杀人|害人|下毒)`, Category: "violence", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-doxxing-zh", Value: `(曝光|泄露|公布|人肉).{0,8}(他人|某人|目标).{0,8}(家庭住址|身份证号|电话号码).{0,10}(骚扰|威胁|报复)`, Category: "privacy_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-explosive-instructions-en", Value: `\b(how (do|can) i|steps? to|guide to|instructions? (for|to))\s+(build|make|assemble)\s+(a\s+)?(bomb|explosive device)\b`, Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-firearm-trafficking-en", Value: `\b(buy|purchase|sell)\s+(an?\s+)?(illegal|unregistered|black[- ]market)\s+(gun|firearm|weapon)s?\b`, Category: "weapons", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-drug-manufacturing-en", Value: `\b(how to|steps? to|guide to)\s+(make|manufacture|synthesize)\s+(meth(amphetamine)?|heroin|fentanyl|cocaine)\b`, Category: "drugs", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-csam-request-en", Value: `\b(download|share|send|sell|buy|give me)\s+(child sexual abuse material|child pornography|csam)(\s+(links?|videos?|images?|sites?))?\b`, Category: "sexual_minors", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-card-fraud-en", Value: `\b(write|build|create|generate|give me)\s+(a\s+)?(credit card skimmer|carding script|credit card theft tool)\b`, Category: "fraud", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-phishing-kit-en", Value: `\b(write|build|create|generate|give me)\s+(a\s+)?(phishing kit|credential stealing page|fake login page)(\s+(code|template|script))?\b`, Category: "fraud", Source: contentFilterSourceBuiltin, Enabled: true},
	// Require a finished malicious artifact, not a detector named after it.
	{ID: "builtin-regex-v1-malware-code-en", Value: `\b(write|build|create|generate|give me)\s+(a\s+)?(ransomware source code|password stealer|keylogger code|banking trojan|credential stealer)($|[.!?,;]|\s+(that|to)\s)`, Category: "malware", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-ddos-service-en", Value: `\b(ddos|denial[- ]of[- ]service)\s+(for hire|rental|attack service|service for sale)\b`, Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-unauthorized-intrusion-en", Value: `\b(hack|break into|take over)\s+(someone else's|another person's|an? unauthorized)\s+(account|server|email|phone|website)\b`, Category: "cyber_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-hitman-en", Value: `\b(hire|find|pay)\s+(a\s+)?(hitman|assassin)\s+(to\s+)?(kill|murder|eliminate)\b`, Category: "violence", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-poison-en", Value: `\b(make|prepare|synthesize)\s+(a\s+)?poison\s+to\s+(kill|harm)\b`, Category: "violence", Source: contentFilterSourceBuiltin, Enabled: true},
	{ID: "builtin-regex-v1-doxxing-en", Value: `\b(find|publish|leak)\s+(someone's|a person's)\s+(home address|phone number|social security number)\s+(to\s+)?(harass|threaten|doxx)\b`, Category: "privacy_abuse", Source: contentFilterSourceBuiltin, Enabled: true},
}, multilingualContentFilterTerms()...)

type compiledContentFilterTerm struct {
	expression *regexp.Regexp
	term       string
	category   string
}

type contentFilterMatcher struct {
	terms []compiledContentFilterTerm
}

func compileContentFilterMatcher(terms []ContentFilterTerm) (*contentFilterMatcher, error) {
	matcher := &contentFilterMatcher{terms: make([]compiledContentFilterTerm, 0, len(terms))}
	for _, term := range terms {
		if !term.Enabled {
			continue
		}
		pattern := strings.TrimSpace(term.Value)
		if pattern == "" {
			continue
		}
		expression, err := regexp.Compile("(?i:" + pattern + ")")
		if err != nil {
			return nil, fmt.Errorf("invalid RE2 expression %q: %w", term.Value, err)
		}
		if expression.MatchString("") {
			return nil, fmt.Errorf("content-filter expression %q must not match empty text", term.Value)
		}
		matcher.terms = append(matcher.terms, compiledContentFilterTerm{expression: expression, term: term.Value, category: term.Category})
	}
	return matcher, nil
}

func (matcher *contentFilterMatcher) Match(value string) ContentFilterMatch {
	if matcher == nil || len(matcher.terms) == 0 {
		return ContentFilterMatch{}
	}
	text := normalizeContentFilterText(value)
	for _, term := range matcher.terms {
		if term.expression.MatchString(text) {
			return ContentFilterMatch{Matched: true, Term: term.term, Category: term.category}
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
		normalized := strings.ToLower(strings.TrimSpace(term.Value))
		validationTerm := term
		validationTerm.Enabled = true
		if _, err := compileContentFilterMatcher([]ContentFilterTerm{validationTerm}); err != nil {
			return ContentFilterSettings{}, err
		}
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
	matcher, err := compileContentFilterMatcher(validated.Terms)
	if err != nil {
		return ContentFilterSettings{}, err
	}
	if err := engine.store.SaveContentFilterSettings(validated); err != nil {
		return ContentFilterSettings{}, err
	}
	engine.contentFilterMu.Lock()
	engine.contentFilterSettings = validated
	engine.contentFilterMatcher = matcher
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
