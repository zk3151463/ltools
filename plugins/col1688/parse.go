package col1688

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// parseHTML 从 1688 页面 HTML 中解析出供应商/工厂记录（名称、电话、地址）。
//
// 采用多策略宽松解析，容忍 1688 前端结构变化：
//  1. 优先解析内嵌 JSON（__INITIAL_DATA__ / __INITIAL_STATE__ / window.__data__），
//     递归收集含 name/company/province/address/mobile/phone 字段的对象。
//  2. 回退到 DOM 启发式：抽取公司名链接文本、tel: 电话、地址关键词文本。
//  3. 统一去重（名称+地址）、过滤空记录、限制条数。
func parseHTML(html string, source string, limit int) []Supplier {
	var items []Supplier
	seen := map[string]bool{}

	add := func(s Supplier) {
		if strings.TrimSpace(s.Name) == "" && strings.TrimSpace(s.Address) == "" && strings.TrimSpace(s.Phone) == "" {
			return
		}
		if s.Time == "" {
			s.Time = nowLocal()
		}
		if s.Source == "" {
			s.Source = source
		}
		key := s.dedupKey()
		if key == "||" || seen[key] {
			return
		}
		seen[key] = true
		items = append(items, s)
	}

	// 策略 1：内嵌 JSON
	embedItems := extractFromEmbeddedJSON(html)
	for _, it := range embedItems {
		add(it)
	}

	// 策略 2：DOM 启发式
	if len(embedItems) == 0 {
		for _, it := range extractFromDOM(html) {
			add(it)
		}
	}

	// 限制条数
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	return items
}

// extractFromEmbeddedJSON 从内嵌的 JSON 数据块中递归提取供应商信息。
func extractFromEmbeddedJSON(html string) []Supplier {
	var out []Supplier
	// 常见的 1688 内嵌数据变量
	keys := []string{
		`window.__INITIAL_DATA__`,
		`window.__INITIAL_STATE__`,
		`window.__data__`,
		`window.__pageData__`,
	}
	for _, k := range keys {
		idx := strings.Index(html, k)
		if idx < 0 {
			continue
		}
		rest := html[idx+len(k):]
		// 跳过等号，定位到 JSON 起点
		rest = strings.TrimLeft(rest, " \t\n\r")
		rest = strings.TrimPrefix(rest, "=")
		rest = strings.TrimLeft(rest, " \t\n\r")
		if !strings.HasPrefix(rest, "{") && !strings.HasPrefix(rest, "[") {
			continue
		}
		// 截取配平的 JSON 子串
		js := extractBalancedJSON(rest)
		if js == "" {
			continue
		}
		// 递归收集
		var raw interface{}
		if err := json.Unmarshal([]byte(js), &raw); err == nil {
			collectJSON(raw, &out)
			if len(out) > 0 {
				break
			}
		}
	}
	return out
}

// extractBalancedJSON 截取从首字符开始、花括号/方括号配平的 JSON 子串。
func extractBalancedJSON(s string) string {
	depth := 0
	inStr := false
	escape := false
	end := -1
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				end = i + 1
			}
		}
		if end > 0 {
			break
		}
	}
	if end <= 0 {
		return ""
	}
	return s[:end]
}

// collectJSON 递归遍历解析后的 JSON，收集看起来像供应商的对象。
func collectJSON(v interface{}, out *[]Supplier) {
	switch node := v.(type) {
	case map[string]interface{}:
		name := pickString(node, "name", "companyName", "companyNameStr", "shopName", "sellerName", "title", "fullName")
		prov := pickString(node, "province", "provinceName")
		city := pickString(node, "city", "cityName")
		town := pickString(node, "town", "townName")
		addr := pickString(node, "address", "fullAddress", "addressDetail")
		phone := pickString(node, "mobile", "phone", "tel", "telephone", "contactPhone", "mobileNo")
		if phone == "" {
			// 可能是嵌套在 contact 子对象里
			if c, ok := node["contact"].(map[string]interface{}); ok {
				phone = pickString(c, "mobile", "phone", "tel")
			}
		}
		main := pickString(node, "mainProduct", "mainCategory", "categoryName", "description")
		level := pickString(node, "level", "strengthTag", "creditLevel", "years", "companyLevel")
		url := pickString(node, "url", "link", "shopUrl", "winportUrl")

		address := strings.TrimSpace(strings.Join([]string{prov, city, town, addr}, " "))
		if name == "" && phone == "" && address == "" {
			// 没有识别出供应商，继续深入子结构
			for _, child := range node {
				collectJSON(child, out)
			}
			return
		}
		// 至少要有名称或电话或地址之一
		*out = append(*out, Supplier{
			Name:    name,
			Phone:   normalizePhone(phone),
			Address: address,
			Main:    main,
			Level:   level,
			URL:     url,
		})
		return
	case []interface{}:
		for _, item := range node {
			collectJSON(item, out)
		}
	}
}

// pickString 从 map 中按多个候选键取第一个非空字符串值。
func pickString(m map[string]interface{}, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch s := v.(type) {
			case string:
				if strings.TrimSpace(s) != "" {
					return s
				}
			case float64:
				return fmt.Sprintf("%v", s)
			}
		}
	}
	return ""
}

var (
	// 中国大陆手机 + 座机。
	// 注意：Go 的 RE2 正则引擎不支持 Perl 风格的前瞻 (?!) 与后顾 (?<!)，
	// 因此这里去掉原 (?![0-9])，并把更长的 11 位手机放在前面优先匹配。
	phoneRe   = regexp.MustCompile(`(?m)(\d{11}|\d{3,4}[- ]?\d{7,8})`)
	// tel: 链接
	telLinkRe = regexp.MustCompile(`(?i)tel\s*:\s*([0-9+\-()]{6,20})`)
	// 公司名常见标签
	companyNameRe = regexp.MustCompile(`(?s)<[^>]+class="[^"]*(?:company-name|text-company|companyName|b2b-company-name|supplier-name|title-line)[^"]*"[^>]*>\s*([^<]{2,40})`)
	// 地址常见标签
	addrTagRe = regexp.MustCompile(`(?s)<[^>]+class="[^"]*(?:text-location|address|location|town-province|detail-address)[^"]*"[^>]*>\s*([^<]{2,60})`)
	// 旺旺/电话文本标签
	contactTextRe = regexp.MustCompile(`(?is)(?:联系电话|联系电话:|电话[:：]?)\s*([0-9+\-()]{6,20})`)
)

// extractFromDOM 用标签/正则启发式从 HTML 抽取供应商信息。
func extractFromDOM(html string) []Supplier {
	var out []Supplier
	// 切分为可能的"卡片"片段：按公司名标签出现的位置分段
	names := companyNameRe.FindAllStringSubmatchIndex(html, -1)
	if len(names) == 0 {
		// 没有公司名标签时，退而使用整页电话/地址拼接（保守，避免空记录）
		return out
	}
	for i, loc := range names {
		start := loc[0]
		end := loc[1]
		if i+1 < len(names) {
			end = names[i+1][0]
		} else {
			end = len(html)
			if end-start > 8000 {
				end = start + 8000
			}
		}
		chunk := html[start:end]

		name := strings.TrimSpace(html[loc[2]:loc[3]])
		if len(name) < 2 || len(name) > 60 {
			name = ""
		}

		phone := firstMatch(chunk, telLinkRe)
		if phone == "" {
			phone = firstMatch(chunk, contactTextRe)
		}
		if phone == "" {
			phone = firstMatch(chunk, phoneRe)
		}
		phone = normalizePhone(phone)

		address := firstMatch(chunk, addrTagRe)
		address = strings.TrimSpace(address)

		if name == "" && phone == "" && address == "" {
			continue
		}
		out = append(out, Supplier{Name: name, Phone: phone, Address: address})
	}
	return out
}

// firstMatch 返回正则第一个捕获组（无捕获组则整段）。
func firstMatch(s string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	if len(m) > 1 {
		return m[1]
	}
	return m[0]
}

// normalizePhone 清洗电话字符串。
func normalizePhone(p string) string {
	p = strings.TrimSpace(p)
	// 去掉中文括号/空格
	r := strings.NewReplacer(" ", "", "（", "(", "）", ")")
	return r.Replace(p)
}

// dedup 对记录去重（名称+地址）。
func dedup(items []Supplier) []Supplier {
	seen := map[string]bool{}
	out := make([]Supplier, 0, len(items))
	for _, s := range items {
		if s.Time == "" {
			s.Time = nowLocal()
		}
		k := s.dedupKey()
		if k == "||" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

