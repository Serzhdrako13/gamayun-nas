// Правила имён новых папок, совместимые с Windows-клиентами SMB.
// Повторяют internal/nas/winname.go и ValidDirName (тексты ошибок — те же);
// общие примеры для обеих реализаций — internal/nas/testdata/winnames.json.
// Окончательная проверка — в gamayund и helper; здесь — подсказка до отправки.
// Тексты — в словарях i18n (раздел names).
import { t } from './i18n/index.js';

const MAX_NAME_BYTES = 255;
const FORBIDDEN = '<>:"/\\|?*';
const RESERVED = new Set(['CON', 'PRN', 'AUX', 'NUL']);
for (const d of ['1', '2', '3', '4', '5', '6', '7', '8', '9', '¹', '²', '³']) {
  RESERVED.add('COM' + d);
  RESERVED.add('LPT' + d);
}

const asciiUpper = (s) => s.replace(/[a-z]/g, (c) => c.toUpperCase());
const hex4 = (cp) => cp.toString(16).toUpperCase().padStart(4, '0');

// Имя устройства Windows (CON, COM1, …), если name зарезервировано: часть до
// первой точки без пробелов в конце, без учёта регистра латиницы; иначе ''.
export function winReservedName(name) {
  let base = name;
  const i = base.indexOf('.');
  if (i >= 0) base = base.slice(0, i);
  base = asciiUpper(base.replace(/ +$/, ''));
  return RESERVED.has(base) ? base : '';
}

// Текст ошибки для имени новой папки или '' — имя допустимо.
// kind: 'dir' — «имя папки» (как в gamayund), 'file' — «имя» (для файлов).
export function dirNameError(name, kind = 'dir') {
  const w = { nom: t(`names.what.${kind}.nom`), gen: t(`names.what.${kind}.gen`) };
  if (name === '') return t('names.empty', w);
  if (new TextEncoder().encode(name).length > MAX_NAME_BYTES) return t('names.tooLong', { ...w, max: MAX_NAME_BYTES });
  if (name === '.' || name === '..') return t('names.dots');
  for (const ch of name) {
    const cp = ch.codePointAt(0);
    if (cp < 0x20 || cp === 0x7f || (cp >= 0x80 && cp <= 0x9f)) return t('names.control', { ...w, cp: hex4(cp) });
    if (FORBIDDEN.includes(ch)) return t('names.forbidden', { ...w, ch });
  }
  if (name.endsWith('.')) return t('names.trailingDot', w);
  if (name.endsWith(' ')) return t('names.trailingSpace', w);
  const dev = winReservedName(name);
  if (dev) return t('names.reserved', { name, dev });
  return '';
}
