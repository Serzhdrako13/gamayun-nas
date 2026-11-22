// Лёгкая i18n без зависимостей. Словари — ru/ (основной) и en/,
// по файлу на раздел (пространство имён); ключ — «раздел.имя[.вложенное]».
// t() читает язык из state.lang; в браузере state — объект $state
// (bindLangState в state.svelte.js), поэтому разметка с t() перерисовывается
// при смене языка. В тестах node — обычный объект, язык по умолчанию ru.
import ru from './ru/index.js';
import en from './en/index.js';

export const DICTS = { ru, en };
export const LANGS = ['ru', 'en'];
export const DEFAULT_LANG = 'ru';
const LOCALES = { ru: 'ru-RU', en: 'en-US' };
const PLURAL = ['one', 'few', 'many', 'other'];

let state = { lang: DEFAULT_LANG };

// bindLangState — подключить реактивное хранилище языка ({lang}).
export function bindLangState(s) {
  state = s;
}

export const lang = () => state.lang;
// locale — для toLocaleString/Intl: 'ru-RU' или 'en-US'.
export const locale = () => LOCALES[state.lang] || LOCALES[DEFAULT_LANG];

// normLang — допустимый язык или ''.
export function normLang(l) {
  const s = String(l || '').toLowerCase();
  return LANGS.find((x) => s === x || s.startsWith(x + '-')) || '';
}

// detectLang — язык браузера (первый в списке), если ru/en, иначе ru.
export function detectLang(list) {
  return normLang((list || [])[0]) || DEFAULT_LANG;
}

// pluralForm — форма множественного числа: ru — one (1, 21), few (2–4, 22–24),
// many (0, 5–20, 11–14…); en — one (1) и other. Дробные — few (ru) / other (en).
export function pluralForm(l, n) {
  const v = Math.abs(Number(n));
  if (l === 'ru') {
    if (!Number.isInteger(v)) return 'few';
    const m10 = v % 10;
    const m100 = v % 100;
    if (m10 === 1 && m100 !== 11) return 'one';
    if (m10 >= 2 && m10 <= 4 && (m100 < 12 || m100 > 14)) return 'few';
    return 'many';
  }
  return v === 1 ? 'one' : 'other';
}

// isPlural — узел словаря с формами множественного числа (лист, а не раздел).
export function isPlural(v) {
  return v != null && typeof v === 'object' && Object.keys(v).length > 0 && Object.keys(v).every((k) => PLURAL.includes(k));
}

function lookup(dict, key) {
  let v = dict;
  for (const p of key.split('.')) {
    if (v == null || typeof v !== 'object') return undefined;
    v = v[p];
  }
  return v;
}

const warned = new Set();

function interpolate(s, params) {
  if (!params) return s;
  return s.replace(/\{(\w+)\}/g, (m, k) => (params[k] != null ? String(params[k]) : m));
}

// tIn — перевод на заданном языке (для тестов и подписей языков).
export function tIn(l, key, params) {
  let v = lookup(DICTS[l], key);
  if (isPlural(v)) {
    const n = params && params.count != null ? params.count : 0;
    v = v[pluralForm(l, n)] ?? v.other ?? v.many;
  }
  if (typeof v !== 'string') {
    if (!warned.has(l + ':' + key)) {
      warned.add(l + ':' + key);
      console.warn(`i18n: нет ключа «${key}» в словаре ${l}`);
    }
    return key;
  }
  return interpolate(v, params);
}

// t — строка интерфейса на текущем языке. params — подстановки {имя};
// count — число для форм множественного числа. Нет ключа — сам ключ и предупреждение.
export function t(key, params) {
  return tIn(state.lang, key, params);
}

// flatKeys — все ключи словаря (формы множественного числа — один лист).
export function flatKeys(dict, prefix = '') {
  const out = [];
  for (const [k, v] of Object.entries(dict)) {
    const key = prefix ? prefix + '.' + k : k;
    if (v != null && typeof v === 'object' && !isPlural(v)) out.push(...flatKeys(v, key));
    else out.push(key);
  }
  return out;
}
