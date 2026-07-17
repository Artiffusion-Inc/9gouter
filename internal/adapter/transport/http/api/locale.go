package api

import (
	"encoding/json"
	"net/http"
)

// supportedLocales mirrors src/i18n/config.js LOCALES.
var supportedLocales = map[string]bool{
	"en": true, "vi": true, "zh-CN": true, "zh-TW": true, "ja": true,
	"pt-BR": true, "pt-PT": true, "ko": true, "es": true, "de": true,
	"fr": true, "he": true, "ar": true, "ru": true, "pl": true,
	"cs": true, "nl": true, "tr": true, "uk": true, "tl": true,
	"id": true, "th": true, "hi": true, "bn": true, "ur": true,
	"ro": true, "sv": true, "it": true, "el": true, "hu": true,
	"fi": true, "da": true, "no": true, "fa": true,
}

const localeCookie = "locale"

// RegisterLocale mounts the public locale cookie route.
func RegisterLocale(mux *http.ServeMux) {
	mux.HandleFunc("OPTIONS /api/locale", localeOptions)
	mux.HandleFunc("POST /api/locale", setLocale)
}

func localeOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.WriteHeader(http.StatusNoContent)
}

func setLocale(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Locale string `json:"locale"`
	}
	if err := parseJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid request body")
		return
	}

	locale := normalizeLocale(body.Locale)
	if !supportedLocales[locale] {
		writeError(w, http.StatusBadRequest, "Invalid locale")
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     localeCookie,
		Value:    locale,
		Path:     "/",
		MaxAge:   60 * 60 * 24 * 365, // 1 year
		HttpOnly: false,
	})
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "locale": locale})
}

func normalizeLocale(locale string) string {
	switch locale {
	case "zh", "zh-CN", "zh-cn":
		return "zh-CN"
	case "zh-TW", "zh-tw":
		return "zh-TW"
	case "pt-BR", "pt-br":
		return "pt-BR"
	case "pt-PT", "pt-pt":
		return "pt-PT"
	}
	if supportedLocales[locale] {
		return locale
	}
	return "en"
}

var _ = json.Marshal
