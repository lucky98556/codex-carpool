package quota

// These are local, conservative request patterns, not OpenAI's moderation model
// or an exhaustive implementation of its policies. Category references:
// https://developers.openai.com/api/docs/guides/moderation
// https://developers.openai.com/api/docs/guides/safety-checks/cybersecurity
// License circumvention is also an operator policy; reverse engineering alone
// is not evidence of abuse. Keep intent and a concrete harmful target together.
type builtinRuleSpec struct {
	name, category, pattern string
}

func multilingualContentFilterTerms() []ContentFilterTerm {
	// A request must start a sentence rather than occur anywhere in a safety
	// explanation or a negation (e.g. "do not bypass license checks"). This is
	// deliberately conservative: do not add a blanket "research" allowlist that
	// would let a harmful request escape by appending a benign keyword.
	const start = `(?:^|[.!?;。！？；]\s*)`
	packs := []struct {
		language, prefix string
		rules            []builtinRuleSpec
	}{
		{"zh", `(请|請)?(帮我|幫我|教我|告诉我如何|告訴我如何|如何|怎么|怎麼|我要|我想)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(制作|製作|生成|做|给我|給我)(非自愿|非自願|未经同意|未經同意)?(色情换脸|色情換臉|换脸色情|換臉色情|换脸裸照|換臉裸照|ai脱衣|ai脫衣|一键脱衣|一鍵脫衣)(视频|影片|图片|圖片|工具|软件|軟體|app)?($|[，。！？；,.!?;]|用来|用來)|把.{1,20}(照片|图片|圖片)(做成|变成|變成|改成)(裸照|色情图|色情圖)|用.{1,20}(照片|人脸|人臉)(做|制作|製作|生成)(色情|裸照)`},
			{"deepfake-fraud", "impersonation_fraud", `(用|利用|通过|通過)(ai)?(换脸|換臉|深度伪造|深度偽造|声音克隆|聲音克隆|语音克隆|語音克隆).{0,24}(骗取|騙取|诈骗|詐騙|骗钱|騙錢|冒充.{1,12}转账|冒充.{1,12}轉帳|绕过.{0,8}(人脸|活体)|繞過.{0,8}(人臉|活體))`},
			{"license", "license_circumvention", `(绕过|繞過|破解|移除|去掉|跳过|跳過|禁用)(这个|這個|软件的?|軟體的?|程序的?|程式的?)?(付费校验|付費校驗|付费验证|付費驗證|授权校验|授權校驗|许可证验证|許可證驗證|激活验证|啟用驗證|会员限制|會員限制|试用限制|試用限制|drm|license\s+checks?)`},
			{"reverse-license", "license_circumvention", `(逆向|反编译|反編譯|脱壳|脫殼).{0,48}(来|來|以便|然后|然後|并|並|后|後|去)(绕过|繞過|破解|移除|跳过|跳過)(付费|付費|授权|授權|许可证|許可證|激活|啟用|试用|試用|会员|會員|drm)`},
			{"keygen", "license_circumvention", `(写|寫|制作|製作|生成|提供|给我|給我)(一个|一個|商业软件的?|商業軟體的?)?(注册机|註冊機|盗版激活码|盜版啟用碼|授权破解补丁|授權破解補丁|license\s+crack|keygen)(源码|源碼|代码|程式碼)?($|[，。！？；,.!?;]|用来|用來)`},
			{"credentials", "credential_theft", `(窃取|竊取|盗取|盜取|偷取|破解)(别人|別人|他人|用户|用戶|受害者)的?(密码|密碼|账号|帳號|凭据|憑據|cookie|会话令牌|工作階段權杖|api\s*key|钱包私钥|錢包私鑰)`},
			{"malware", "malware", `(写|寫|制作|製作|开发|開發|生成)(一个|一個)?(勒索软件|勒索軟體|密码窃取器|密碼竊取器|远控木马|遠控木馬|窃密木马|竊密木馬)(源码|源碼|代码|代碼|程式碼|程序|程式)?($|[，。！？；,.!?;]|用来|用來)`},
			{"evasion", "cyber_abuse", `(修改|改造|混淆|加壳|加殼)(我的|这个|這個)?(木马|木馬|勒索软件|勒索軟體|窃取器|竊取器).{0,24}(免杀|免殺|绕过检测|繞過偵測|躲避杀毒|躲避防毒|逃避edr)`},
			{"weapons", "weapons", `(给我|給我|提供|制作|製作|制造|製造|合成)(自制|自製)?(炸弹|炸彈|爆炸装置|爆炸裝置|化学武器|化學武器|生物武器|神经毒剂|神經毒劑)(的?(配方|教程|步骤|步驟|方法))?`},
			{"minors", "sexual_minors", `(生成|制作|製作|画|畫|下载|下載|发送|發送|给我|給我)(儿童|兒童|未成年|未滿18歲|未满18岁).{0,8}(色情|性爱|性愛|性虐待|裸照)`},
			{"self-harm", "self_harm", `(告诉我|告訴我|给我|給我|提供|教我)(自杀|自殺|自残|自殘)(的?(具体|具體|详细|詳細|无痛|無痛|有效))?(方法|步骤|步驟|教程)`},
			{"fraud", "fraud", `(写|寫|制作|製作|生成|给我|給我)(诈骗话术|詐騙話術|洗钱教程|洗錢教程|伪造护照教程|偽造護照教程|盗刷脚本|盜刷腳本|勒索信)($|[，。！？；,.!?;]|用来|用來|发给|發給)`},
			{"privacy", "privacy_abuse", `(帮我|幫我)?(人肉|开盒|開盒|跟踪|跟蹤|定位)(这个人|這個人|他人|别人|別人|前女友|前男友).{0,16}(住址|地址|位置|身份证|身分證|手机号|手機號)`},
			{"sexual-violence", "sexual_violence", `(生成|写|寫|制作|製作)(色情|情色)?(强奸|強姦|性暴力|非自愿性行为|非自願性行為)(色情|情色|视频|影片|故事|图片|圖片)`},
			{"sexual", "sexual", `(写|寫|生成|制作|製作)(一篇|一段|一张|一張)?(露骨色情|露骨性爱|露骨性愛)(小说|小說|故事|图片|圖片|视频|影片)`},
			{"hate", "hate", `(写|寫|生成|编写|編寫|传播|傳播)(煽动|煽動|鼓吹|宣扬|宣揚)(种族仇恨|種族仇恨|民族仇恨|宗教仇恨|种族灭绝|種族滅絕)(的?(文章|宣传|宣傳|口号|口號|言论|言論))?`},
			{"harassment", "harassment", `(写|寫|生成|发送|發送)(一封|一条|一條)?(死亡威胁|死亡威脅|人身威胁|人身威脅|骚扰短信|騷擾簡訊|恐吓信|恐嚇信)($|[，。！？；,.!?;]|给|給|发给|發給)`},
			{"terrorism", "violence", `(策划|策劃|组织|組織|实施|實施)(一起|一次)?(恐怖袭击|恐怖襲擊|校园枪击|校園槍擊|自杀式袭击|自殺式襲擊)|为(恐怖组织|恐怖組織)(招募|募集资金|募集資金)`},
			{"trafficking", "illicit", `(给我|給我|提供|策划|策劃)(人口贩卖|人口販賣|贩卖人口|販賣人口|绑架勒索|綁架勒索|贩卖人体器官|販賣人體器官)(的?(方案|渠道|步骤|步驟|教程))`},
		}},
		{"en", `(?:please\s+)?(?:(?:how (?:do|can) i|how to|help me(?: to)?|i want to|can you|could you)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:create|make|generate|send me)\s+(?:a\s+)?(?:non[- ]consensual\s+)?(?:deepfake porn|pornographic face[- ]swap|nudify app|undress app)(?:$|[.!?,;]|\s+(?:of|using|with)\s)|(?:undress|nudify)\s+(?:this person|her|him|someone)\b|(?:turn|convert)\s+(?:her|his|this person's)\s+photo\s+into\s+(?:a\s+)?nude\b`},
			{"deepfake-fraud", "impersonation_fraud", `(?:use|create)\s+(?:a\s+)?(?:deepfake|face[- ]swap|voice clone)\s+to\s+(?:scam|defraud|trick someone into paying|impersonate .{1,24} to steal|bypass (?:facial|face|liveness|identity) verification)\b`},
			{"license", "license_circumvention", `(?:bypass|crack|remove|disable|patch out)\s+(?:the\s+)?(?:software\s+)?(?:license|licence|activation|subscription|payment|trial)\s+(?:checks?|verification|restrictions?|limits?|validation)\b|(?:bypass|crack|remove)\s+(?:the\s+)?drm\b`},
			{"reverse-license", "license_circumvention", `(?:reverse[- ]engineer|decompile|unpack)\s+.{1,64}\s+(?:to|and)\s+(?:bypass|remove|crack|disable)\s+(?:the\s+)?(?:license|licence|activation|payment|trial|drm)\b`},
			{"keygen", "license_circumvention", `(?:write|build|generate|give me)\s+(?:a\s+)?(?:license crack|pirated activation key|keygen for commercial software)\b`},
			{"credentials", "credential_theft", `(?:steal|extract|crack)\s+(?:someone else's|another person's|victims'?|users'|stolen)\s+(?:passwords?|credentials?|cookies?|session tokens?|api keys?|wallet private keys?)\b`},
			{"malware", "malware", `(?:write|build|create|generate)\s+(?:a\s+)?(?:ransomware|credential stealer|password stealer|banking trojan|botnet malware)(?:\s+(?:source code|code|program))?(?:$|[.!?,;]|\s+(?:that|to)\s)`},
			{"evasion", "cyber_abuse", `(?:modify|obfuscate|pack)\s+(?:my\s+|this\s+)?(?:malware|ransomware|trojan)\s+to\s+(?:evade|bypass|disable)\s+(?:antivirus|edr|detection)\b`},
			{"weapons", "weapons", `(?:give me|provide)\s+(?:a\s+)?(?:recipe|instructions|steps)\s+(?:for|to)\s+(?:making|building|make|build)\s+(?:a\s+)?(?:bomb|explosive device|chemical weapon|biological weapon|nerve agent)\b`},
			{"minors", "sexual_minors", `(?:create|generate|draw|send|give me)\s+(?:an?\s+)?(?:sexual|pornographic|nude)\s+(?:images?|videos?|pictures?|stories)\s+(?:of|about|depicting)\s+(?:a\s+)?(?:child|minor|underage person)\b`},
			{"self-harm", "self_harm", `(?:give me|provide|tell me)\s+(?:detailed\s+|painless\s+|effective\s+)?(?:suicide methods|self[- ]harm instructions|steps to kill myself)\b|(?:how to|help me)\s+(?:kill myself|commit suicide)\b`},
			{"fraud", "fraud", `(?:write|create|give me|provide)\s+(?:a\s+)?(?:scam script|blackmail letter|money laundering instructions|passport forgery instructions)(?:$|[.!?,;]|\s+(?:that|to)\s)`},
			{"privacy", "privacy_abuse", `(?:doxx?|stalk|secretly track)\s+(?:this person|my ex|someone|a private person)\b`},
			{"sexual-violence", "sexual_violence", `(?:write|create|generate)\s+(?:an?\s+)?(?:pornographic|erotic)\s+(?:story|video|image)\s+(?:about|of|depicting)\s+(?:rape|sexual assault|non[- ]consensual sex)\b`},
			{"sexual", "sexual", `(?:write|create|generate)\s+(?:an?\s+)?(?:sexually explicit|pornographic)\s+(?:story|image|video)\b`},
			{"hate", "hate", `(?:write|generate|spread)\s+(?:a\s+)?(?:speech|propaganda|slogans?)\s+(?:promoting|inciting|advocating)\s+(?:racial hatred|religious hatred|genocide|ethnic cleansing)\b`},
			{"harassment", "harassment", `(?:write|send|generate)\s+(?:a\s+)?(?:death threat|blackmail threat|harassing message)(?:$|[.!?,;]|\s+(?:that|to)\s)`},
			{"terrorism", "violence", `(?:plan|organize|carry out)\s+(?:a\s+)?(?:terrorist attack|school shooting|suicide bombing)\b|recruit\s+(?:members|fighters)\s+for\s+(?:a\s+)?terrorist\s+(?:group|organization)\b`},
			{"trafficking", "illicit", `(?:give me|provide)\s+(?:a\s+)?(?:plan|instructions|steps)\s+for\s+(?:human trafficking|organ trafficking|kidnapping for ransom)\b`},
		}},
		{"ja", `(?:お願い[、, ]*)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:ディープフェイクポルノ|性的な顔交換画像|他人の脱衣画像)を(?:作って|生成して|送って)`},
			{"deepfake-fraud", "impersonation_fraud", `(?:顔交換|ディープフェイク|音声クローン)で.{0,20}(?:金を騙し取る|送金させる|顔認証を突破|本人確認を回避)`},
			{"license", "license_circumvention", `(?:ソフトの)?(?:ライセンス認証|課金チェック|有料制限|試用制限|drm)を(?:回避|解除|突破|無効化)(?:する方法|して|したい|するコード)`},
			{"credentials", "credential_theft", `(?:他人|被害者)の(?:パスワード|認証情報|cookie|秘密鍵)を(?:盗む方法|盗んで|盗みたい)`},
			{"malware", "malware", `(?:ランサムウェア|パスワード窃取ツール|情報窃取マルウェア)(?:のコード)?を(?:作って|書いて|作成して)`},
			{"weapons", "weapons", `(?:爆弾|化学兵器|生物兵器)の(?:作り方|製造方法|製造手順)を(?:教えて|説明して)`},
			{"minors", "sexual_minors", `(?:児童|未成年)の(?:ポルノ|性的な画像|性的な動画)を(?:作って|送って|生成して)`},
			{"self-harm", "self_harm", `(?:自殺|自傷)の(?:具体的な方法|手順|方法)を教えて`},
		}},
		{"ko", `(?:제발\s*)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:딥페이크 포르노|비동의 합성 음란물|타인의 누드 합성 사진)(?:를|을)?\s*(?:만들어줘|생성해줘|보내줘)`},
			{"deepfake-fraud", "impersonation_fraud", `(?:딥페이크|얼굴 합성|음성 복제)(?:로|를 이용해)\s*.{0,20}(?:돈을 가로채|송금하게|얼굴 인증을 우회|본인 인증을 우회)`},
			{"license", "license_circumvention", `(?:라이선스 인증|유료 인증|결제 확인|체험판 제한|drm)(?:을|를)?\s*(?:우회|해제|무력화)(?:하는 방법|해 줘|해줘|하고 싶어)`},
			{"credentials", "credential_theft", `(?:타인|다른 사람|피해자)의\s*(?:비밀번호|인증 정보|쿠키|개인 키)(?:를|을)?\s*(?:훔치는 방법|훔쳐 줘|훔쳐줘)`},
			{"malware", "malware", `(?:랜섬웨어|비밀번호 탈취 프로그램|정보 탈취 악성코드)(?:를|을)?\s*(?:만들어 줘|만들어줘|작성해 줘)`},
			{"weapons", "weapons", `(?:폭탄|화학 무기|생물 무기)(?:을|를)?\s*(?:만드는 방법|제조 방법)(?:을|를)?\s*알려줘`},
			{"minors", "sexual_minors", `(?:아동|미성년자)의?\s*(?:포르노|성적인 이미지|성적인 영상)(?:를|을)?\s*(?:생성해줘|만들어줘|보내줘)`},
			{"self-harm", "self_harm", `(?:자살|자해)\s*(?:방법|구체적인 방법|절차)(?:을|를)?\s*알려줘`},
		}},
		{"ru", `(?:(?:пожалуйста|помоги мне|как)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:создай|сделай|пришли)\s+(?:дипфейк-порно|порно с заменой лица|обнажённое фото без согласия)`},
			{"deepfake-fraud", "impersonation_fraud", `используй\s+(?:дипфейк|клон голоса)\s+для\s+(?:кражи денег|обхода проверки лица|мошенничества)`},
			{"license", "license_circumvention", `(?:обойти|взломать|отключить)\s+(?:проверку лицензии|активацию программы|платную защиту|ограничение пробной версии|drm)`},
			{"credentials", "credential_theft", `(?:украсть|укради|взломать)\s+(?:чужие пароли|чужой пароль|данные входа жертвы|чужие приватные ключи)`},
			{"malware", "malware", `(?:напиши|создай|написать|создать)\s+(?:вирус-вымогатель|программу для кражи паролей|банковский троян)`},
			{"weapons", "weapons", `(?:изготовить|сделать)\s+(?:самодельную бомбу|бомбу|химическое оружие|биологическое оружие)`},
			{"minors", "sexual_minors", `(?:создай|пришли|скачать|создать)\s+(?:детскую порнографию|сексуальные изображения несовершеннолетних)`},
			{"self-harm", "self_harm", `(?:дай|расскажи)\s+(?:способы самоубийства|инструкции по самоубийству)`},
		}},
		{"es", `(?:(?:por favor|ayúdame a|ayudame a|cómo|como)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:crea|genera|envía)\s+(?:un\s+)?(?:deepfake pornográfico|desnudo sin consentimiento|porno con cambio de cara)`},
			{"deepfake-fraud", "impersonation_fraud", `usa\s+(?:un\s+)?(?:deepfake|clon de voz)\s+para\s+(?:estafar|robar dinero|eludir la verificación facial)`},
			{"license", "license_circumvention", `(?:eludir|saltarse|desactivar|elimina)\s+(?:la\s+|las\s+|el\s+)?(?:verificación de licencia|verificacion de licencia|restricciones de pago|activación del software|drm)`},
			{"credentials", "credential_theft", `(?:robar|roba)\s+(?:las\s+)?(?:contraseñas ajenas|credenciales de otras personas|claves privadas de otras personas)`},
			{"malware", "malware", `(?:crea|crear|escribe|escribir)\s+(?:un\s+)?(?:ransomware|ladrón de contraseñas|troyano bancario)`},
			{"weapons", "weapons", `(?:fabricar|construir|hacer)\s+(?:una\s+)?(?:bomba casera|bomba|arma química|arma biológica)`},
			{"minors", "sexual_minors", `(?:crea|genera|envía|descargar)\s+(?:pornografía infantil|imagenes sexuales de menores|imágenes sexuales de menores)`},
			{"self-harm", "self_harm", `(?:dame|proporciona)\s+(?:métodos de suicidio|instrucciones para suicidarme)`},
		}},
		{"fr", `(?:(?:s'il te plaît|aide-moi à|comment)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:crée|génère|envoie)\s+(?:un\s+)?(?:deepfake pornographique|nu sans consentement|porno avec échange de visage)`},
			{"deepfake-fraud", "impersonation_fraud", `utilise\s+(?:un\s+)?(?:deepfake|clone vocal)\s+pour\s+(?:escroquer|voler de l'argent|contourner la vérification faciale)`},
			{"license", "license_circumvention", `(?:contourner|désactiver|supprimer)\s+(?:la\s+|les\s+|le\s+)?(?:vérification de licence|restrictions payantes|activation du logiciel|drm)`},
			{"credentials", "credential_theft", `(?:voler|vole)\s+(?:les\s+)?(?:mots de passe d'autrui|identifiants des victimes|clés privées d'autrui)`},
			{"malware", "malware", `(?:crée|créer|écris|écrire)\s+(?:un\s+)?(?:rançongiciel|voleur de mots de passe|cheval de troie bancaire)`},
			{"weapons", "weapons", `(?:fabriquer|construire)\s+(?:une\s+)?(?:bombe|arme chimique|arme biologique)`},
			{"minors", "sexual_minors", `(?:crée|génère|envoie|télécharge)\s+(?:de la\s+|des\s+)?(?:pédopornographie|images sexuelles de mineurs)`},
			{"self-harm", "self_harm", `(?:donne-moi|fournis)\s+(?:des\s+)?(?:méthodes de suicide|instructions pour me suicider)`},
		}},
		{"de", `(?:(?:bitte|hilf mir)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:erstelle|generiere|sende)\s+(?:einen?\s+)?(?:deepfake-porno|nacktbild ohne einwilligung|porno mit gesichtstausch)`},
			{"deepfake-fraud", "impersonation_fraud", `nutze\s+(?:einen?\s+)?(?:deepfake|stimmklon)\s+(?:um geld zu stehlen|für betrug|zum umgehen der gesichtsprüfung)`},
			{"license", "license_circumvention", `(?:umgehe|entferne|deaktiviere)\s+(?:die\s+|den\s+|das\s+)?(?:lizenzprüfung|lizenzpruefung|bezahlbeschränkung|softwareaktivierung|drm)`},
			{"credentials", "credential_theft", `(?:stiehl|knacke)\s+(?:fremde passwörter|fremde passwoerter|zugangsdaten anderer|private schlüssel anderer)`},
			{"malware", "malware", `(?:schreibe|erstelle|programmiere)\s+(?:einen?\s+)?(?:ransomware|passwort-stealer|banking-trojaner)`},
			{"weapons", "weapons", `(?:baue|erstelle)\s+(?:eine\s+)?(?:bombe|chemische waffe|biologische waffe)`},
			{"minors", "sexual_minors", `(?:erstelle|generiere|sende)\s+(?:kinderpornografie|sexuelle bilder von minderjährigen)`},
			{"self-harm", "self_harm", `(?:gib mir|erkläre mir)\s+(?:suizidmethoden|eine anleitung zum selbstmord)`},
		}},
		{"pt", `(?:(?:por favor|ajuda-me a|como)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:crie|gere|envie)\s+(?:um\s+)?(?:deepfake pornográfico|nu sem consentimento|pornô com troca de rosto)`},
			{"deepfake-fraud", "impersonation_fraud", `use\s+(?:um\s+)?(?:deepfake|clone de voz)\s+para\s+(?:roubar dinheiro|aplicar golpes|burlar a verificação facial)`},
			{"license", "license_circumvention", `(?:burlar|contornar|remover)\s+(?:a\s+|as\s+|o\s+)?(?:verificação de licença|verificacao de licenca|restrições de pagamento|ativação do software|drm)`},
			{"credentials", "credential_theft", `(?:roubar|roube)\s+(?:as\s+)?(?:senhas de outras pessoas|credenciais de vítimas|chaves privadas de outras pessoas)`},
			{"malware", "malware", `(?:crie|criar|escreva|escrever)\s+(?:um\s+)?(?:ransomware|roubador de senhas|trojan bancário)`},
			{"weapons", "weapons", `(?:fabricar|construir|fazer)\s+(?:uma\s+)?(?:bomba caseira|bomba|arma química|arma biológica)`},
			{"minors", "sexual_minors", `(?:crie|gere|envie|baixar)\s+(?:pornografia infantil|imagens sexuais de menores)`},
			{"self-harm", "self_harm", `(?:me dê|dê-me|forneça)\s+(?:métodos de suicídio|instruções para me suicidar)`},
		}},
		{"it", `(?:(?:per favore|aiutami a|come)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:crea|genera|invia)\s+(?:un\s+)?(?:deepfake pornografico|nudo senza consenso|porno con scambio di volto)`},
			{"deepfake-fraud", "impersonation_fraud", `usa\s+(?:un\s+)?(?:deepfake|clone vocale)\s+per\s+(?:rubare denaro|truffare|aggirare la verifica facciale)`},
			{"license", "license_circumvention", `(?:aggirare|rimuovere|disattivare)\s+(?:la\s+|le\s+|il\s+)?(?:verifica della licenza|restrizioni di pagamento|attivazione del software|drm)`},
			{"credentials", "credential_theft", `(?:rubare|ruba)\s+(?:le\s+)?(?:password altrui|credenziali delle vittime|chiavi private altrui)`},
			{"malware", "malware", `(?:crea|creare|scrivi|scrivere)\s+(?:un\s+)?(?:ransomware|ladro di password|trojan bancario)`},
			{"weapons", "weapons", `(?:fabbricare|costruire)\s+(?:una\s+)?(?:bomba|arma chimica|arma biologica)`},
			{"minors", "sexual_minors", `(?:crea|genera|invia|scarica)\s+(?:pornografia infantile|immagini sessuali di minori)`},
			{"self-harm", "self_harm", `(?:dammi|fornisci)\s+(?:metodi di suicidio|istruzioni per suicidarmi)`},
		}},
		{"ar", `(?:(?:من فضلك|كيف يمكنني|كيف)\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:أنشئ|اصنع|أرسل)\s+(?:تزييف عميق إباحي|صورة عارية بدون موافقة|صور إباحية بتبديل الوجه)`},
			{"deepfake-fraud", "impersonation_fraud", `استخدم\s+(?:التزييف العميق|استنساخ الصوت)\s+(?:لسرقة المال|للاحتيال|لتجاوز التحقق من الوجه)`},
			{"license", "license_circumvention", `(?:تجاوز|تعطيل|إزالة)\s+(?:التحقق من الترخيص|فحص الترخيص|قيود الدفع|تفعيل البرنامج|حماية drm)`},
			{"credentials", "credential_theft", `(?:سرقة|اسرق)\s+(?:كلمات مرور الآخرين|بيانات دخول الضحايا|المفاتيح الخاصة للآخرين)`},
			{"malware", "malware", `(?:اكتب|أنشئ|إنشاء)\s+(?:برنامج فدية|برمجية فدية|برنامج لسرقة كلمات المرور)`},
			{"weapons", "weapons", `(?:صنع|تصنيع)\s+(?:قنبلة|سلاح كيميائي|سلاح بيولوجي)`},
			{"minors", "sexual_minors", `(?:أنشئ|أرسل|اعطني)\s+(?:صور جنسية للأطفال|مواد إباحية للأطفال)`},
			{"self-harm", "self_harm", `(?:أعطني|اعطني)\s+(?:طرق الانتحار|تعليمات الانتحار)`},
		}},
		{"hi", `(?:कृपया\s+)?`, []builtinRuleSpec{
			{"deepfake-sexual", "sexual_exploitation", `(?:डीपफेक पोर्न|बिना सहमति की नग्न तस्वीर|चेहरा बदलकर अश्लील तस्वीर)\s+(?:बनाओ|भेजो)`},
			{"deepfake-fraud", "impersonation_fraud", `(?:डीपफेक|आवाज क्लोनिंग)\s+से\s+(?:पैसे चुराओ|धोखाधड़ी करो|चेहरे का सत्यापन बायपास करो)`},
			{"license", "license_circumvention", `(?:लाइसेंस जाँच|लाइसेंस जांच|सॉफ्टवेयर सक्रियण|भुगतान प्रतिबंध|drm)\s+(?:को\s+)?(?:बायपास|हटाने|निष्क्रिय)\s+(?:करो|करने का तरीका|का तरीका)`},
			{"credentials", "credential_theft", `(?:दूसरों के पासवर्ड|दूसरे के पासवर्ड|पीड़ितों की निजी कुंजी)\s+(?:चुराओ|चुराने का तरीका)`},
			{"malware", "malware", `(?:रैंसमवेयर|पासवर्ड चुराने वाला प्रोग्राम)\s+(?:बनाओ|लिखो)`},
			{"weapons", "weapons", `(?:बम|रासायनिक हथियार|जैविक हथियार)\s+बनाने का तरीका\s+बताओ`},
			{"minors", "sexual_minors", `(?:बच्चों की अश्लील तस्वीरें|नाबालिगों की यौन तस्वीरें)\s+(?:बनाओ|भेजो)`},
			{"self-harm", "self_harm", `(?:आत्महत्या के तरीके|खुद को मारने के तरीके)\s+बताओ`},
		}},
	}
	var terms []ContentFilterTerm
	for _, pack := range packs {
		for _, rule := range pack.rules {
			terms = append(terms, ContentFilterTerm{
				ID:       "builtin-regex-v2-" + rule.name + "-" + pack.language,
				Value:    start + pack.prefix + "(?:" + rule.pattern + ")",
				Category: rule.category, Source: contentFilterSourceBuiltin, Enabled: true,
			})
		}
	}
	return terms
}
