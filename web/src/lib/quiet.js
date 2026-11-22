// Тишина уведомлений — расчёт срока и подписи (без DOM, для тестов).
import { t, locale } from './i18n/index.js';

// Подпись — геттер: читается на текущем языке при каждой отрисовке.
export const QUIET_PRESETS = ['1h', '4h', 'tomorrow', 'custom'].map((k) =>
  Object.defineProperty([k], 1, { get: () => t('quiet.preset.' + k), enumerable: true }),
);
export const QUIET_MAX_MS = 7 * 24 * 3600 * 1000;

// quietUntil — конец тишины (Date) или null, если срок неверен.
// custom — значение <input type="datetime-local"> (местное время браузера).
export function quietUntil(preset, now = new Date(), custom = '') {
  let t;
  if (preset === '1h') t = new Date(now.getTime() + 3600e3);
  else if (preset === '4h') t = new Date(now.getTime() + 4 * 3600e3);
  else if (preset === 'tomorrow') {
    t = new Date(now);
    t.setDate(t.getDate() + 1);
    t.setHours(9, 0, 0, 0);
  } else t = custom ? new Date(custom) : null;
  if (!t || isNaN(t) || t <= now || t - now > QUIET_MAX_MS) return null;
  return t;
}

// quietText — строка состояния действующей тишины.
export function quietText(q, fmt = (x) => new Date(x).toLocaleString(locale())) {
  if (!q) return t('quiet.text.off');
  let s = t('quiet.text.until', { until: fmt(q.until), reason: q.reason });
  s += q.mute_critical ? t('quiet.text.criticalMuted') : t('quiet.text.criticalSent');
  s += t('quiet.text.suppressed', { count: q.suppressed || 0 });
  if (q.suppressed_critical) s += t('quiet.text.suppressedCritical', { count: q.suppressed_critical });
  return s + '.';
}
