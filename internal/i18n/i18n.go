package i18n

import (
	"embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed locales.json
var localeFS embed.FS

var locales map[string]map[string]interface{}

func init() {
	data, err := localeFS.ReadFile("locales.json")
	if err != nil {
		panic("i18n: cannot read embedded locales.json: " + err.Error())
	}
	if err := json.Unmarshal(data, &locales); err != nil {
		panic("i18n: cannot parse locales.json: " + err.Error())
	}
}

// SupportedLocales returns the list of supported locale codes.
// PHP: app_i18n_supported_locales()
func SupportedLocales() []string {
	return []string{"en", "zh", "ja", "ko", "es"}
}

// NormalizeLocale normalizes a locale string to a supported code.
// Returns "" if not recognized.
// PHP: app_i18n_normalize_locale()
func NormalizeLocale(locale string) string {
	v := strings.ToLower(strings.TrimSpace(locale))
	if v == "" {
		return ""
	}
	if strings.HasPrefix(v, "zh") {
		return "zh"
	}
	if strings.HasPrefix(v, "en") {
		return "en"
	}
	if strings.HasPrefix(v, "ja") {
		return "ja"
	}
	if strings.HasPrefix(v, "ko") {
		return "ko"
	}
	if strings.HasPrefix(v, "es") {
		return "es"
	}
	return ""
}

// IsSupported checks if a locale code is in the supported list.
func IsSupported(locale string) bool {
	for _, s := range SupportedLocales() {
		if s == locale {
			return true
		}
	}
	return false
}

// ResolveLocale determines the locale from query param, cookie, or Accept-Language.
// PHP: app_i18n_resolve_locale()
func ResolveLocale(r *http.Request) string {
	// 1. ?lang= query param
	queryLocale := NormalizeLocale(r.URL.Query().Get("lang"))
	if queryLocale != "" && IsSupported(queryLocale) {
		return queryLocale
	}

	// 2. Cookie
	cookieLocale := NormalizeLocale(getCookie(r, "lang"))
	if cookieLocale != "" && IsSupported(cookieLocale) {
		return cookieLocale
	}

	// 3. Accept-Language header
	acceptLanguage := strings.ToLower(r.Header.Get("Accept-Language"))
	if strings.Contains(acceptLanguage, "zh") {
		return "zh"
	}
	if strings.Contains(acceptLanguage, "ko") {
		return "ko"
	}
	if strings.Contains(acceptLanguage, "es") {
		return "es"
	}
	if strings.Contains(acceptLanguage, "ja") {
		return "ja"
	}

	return "en"
}

// SetLangCookie sets the lang cookie on the response (when ?lang= is used).
func SetLangCookie(w http.ResponseWriter, locale string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "lang",
		Value:    locale,
		MaxAge:   31536000,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
	})
}

// Translate looks up a dotted key in the locale catalog.
// PHP: app_t(key, vars, locale)
// Returns the key itself if not found.
func Translate(locale, key string) string {
	catalog, ok := locales[locale]
	if !ok {
		catalog = locales["en"]
	}
	val := lookup(catalog, key)
	if val == "" {
		// Fallback to en
		if locale != "en" {
			val = lookup(locales["en"], key)
		}
	}
	if val == "" {
		return key
	}
	return val
}

// T is a shorthand for Translate.
func T(locale, key string) string {
	return Translate(locale, key)
}

// Scope returns a sub-catalog as a flat map for a given scope prefix.
// Used to pass i18n data to JavaScript (window.OWNER_I18N).
// PHP: app_i18n_scope(scope, locale)
func Scope(locale, scope string) map[string]interface{} {
	catalog, ok := locales[locale]
	if !ok {
		catalog = locales["en"]
	}
	if scoped, ok := catalog[scope].(map[string]interface{}); ok {
		return scoped
	}
	return make(map[string]interface{})
}

// LanguageOptions returns the language selector options.
// PHP: app_i18n_language_options(displayLocale)
func LanguageOptions(displayLocale string) []map[string]string {
	if displayLocale == "" {
		displayLocale = "en"
	}
	var options []map[string]string
	for _, code := range SupportedLocales() {
		label := Translate(displayLocale, "common.lang_"+code)
		options = append(options, map[string]string{
			"code":  code,
			"label": label,
		})
	}
	return options
}

// LocaleURL builds a URL with ?lang=locale appended.
// PHP: app_i18n_locale_url(locale, path)
func LocaleURL(locale, path string) string {
	normalized := NormalizeLocale(locale)
	if normalized == "" {
		normalized = "en"
	}
	if path == "" {
		path = "/"
	}
	// Split path and query
	parts := strings.SplitN(path, "?", 2)
	basePath := parts[0]
	query := ""
	if len(parts) > 1 {
		query = parts[1]
	}

	// Parse existing query params
	params := parseQuery(query)
	params["lang"] = normalized

	queryStr := buildQuery(params)
	if queryStr == "" {
		return basePath
	}
	return basePath + "?" + queryStr
}

// lookup traverses a nested map using dot notation.
func lookup(catalog map[string]interface{}, key string) string {
	parts := strings.Split(key, ".")
	var current interface{} = catalog
	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return ""
		}
		current, ok = m[part]
		if !ok {
			return ""
		}
	}
	if s, ok := current.(string); ok {
		return s
	}
	return ""
}

func getCookie(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

func parseQuery(q string) map[string]string {
	params := make(map[string]string)
	if q == "" {
		return params
	}
	for _, pair := range strings.Split(q, "&") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) == 2 {
			params[kv[0]] = kv[1]
		}
	}
	return params
}

func buildQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	var parts []string
	for k, v := range params {
		parts = append(parts, k+"="+v)
	}
	return strings.Join(parts, "&")
}
