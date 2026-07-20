export const LOCALES = [
  "en",
  "zh-CN",
  "es",
];
export const DEFAULT_LOCALE = "en";
export const LOCALE_COOKIE = "locale";

export const LOCALE_NAMES = {
  en: "English",
  "zh-CN": "简体中文",
  es: "Español",
};

export function normalizeLocale(locale) {
  if (locale === "zh" || locale === "zh-CN") {
    return "zh-CN";
  }
  if (locale === "en") {
    return "en";
  }
  if (locale === "es") {
    return "es";
  }
  return DEFAULT_LOCALE;
}

export function isSupportedLocale(locale) {
  return LOCALES.includes(locale);
}