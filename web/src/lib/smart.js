// SMART: итог диска и графики истории. Чистые функции —
// проверяются в lib/smart.test.js.
import { t } from './i18n/index.js';

// smartBadge — [текст, класс бейджа] по итогу последнего опроса (d.smart).
export function smartBadge(s) {
  if (!s) return [t('smart.badge.notPolled'), ''];
  // SMART есть, но выключен в диске.
  if (s.disabled) return s.manual_off ? [t('smart.badge.manualOff'), ''] : [t('smart.badge.disabled'), 'warn'];
  if (!s.supported) return [t('smart.badge.noSmart'), ''];
  const tc = s.temperature_c != null ? ' · ' + s.temperature_c + ' °C' : '';
  switch (s.level) {
    case 'ok': return [t('common.inNorm') + tc, 'ok'];
    case 'warning': return [t('smart.badge.warning') + tc, 'warn'];
    case 'critical': return [t('smart.badge.critical') + tc, 'bad'];
  }
  return [s.standby ? t('smart.badge.standby') : t('smart.badge.noData'), ''];
}

// smartCounts — сводка по снимку дисков для «Панели»: {ok, warning,
// critical, unknown, maxTemp, problems: [{name, level, reasons}]}.
export function smartCounts(disks) {
  const out = { ok: 0, warning: 0, critical: 0, unknown: 0, maxTemp: null, problems: [] };
  for (const d of disks || []) {
    const s = d.smart;
    // Выключен не оператором (включить не удалось) — внимание.
    if (s?.disabled && !s.manual_off) {
      out.warning++;
      out.problems.push({ name: d.name, level: 'warning', reasons: [t('smart.disabledReason')] });
      continue;
    }
    const lv = s && s.supported && ['ok', 'warning', 'critical'].includes(s.level) ? s.level : 'unknown';
    out[lv]++;
    if (s?.temperature_c != null && (out.maxTemp == null || s.temperature_c > out.maxTemp)) out.maxTemp = s.temperature_c;
    if (lv === 'warning' || lv === 'critical') out.problems.push({ name: d.name, level: lv, reasons: s.reasons || [] });
  }
  out.problems.sort((a, b) => (a.level === b.level ? 0 : a.level === 'critical' ? -1 : 1));
  return out;
}

// scale — функция значения в координату: [lo, hi] → [h - pad, pad].
function scale(lo, hi, h, pad) {
  const span = hi > lo ? hi - lo : 1;
  return (v) => h - pad - ((v - lo) / span) * (h - 2 * pad);
}

// series — полилинии (points) значения key по времени в рамке w×h за
// окно [from, to] (мс). Пропуск значения или разрыв больше gap (мс) —
// новая полилиния. Пределы оси Y — lo/hi (расширяются по данным).
export function series(points, key, { from, to, w, h, lo, hi, gap = 0, pad = 2 }) {
  const vals = (points || []).filter((p) => p[key] != null);
  for (const p of vals) {
    lo = Math.min(lo, p[key]);
    hi = Math.max(hi, p[key]);
  }
  const y = scale(lo, hi, h, pad);
  const span = to > from ? to - from : 1;
  const x = (t) => ((t - from) / span) * w;
  const lines = [];
  let cur = [];
  let prev = null;
  for (const p of points || []) {
    const t = new Date(p.t).getTime();
    if (p[key] == null || (gap && prev != null && t - prev > gap)) {
      if (cur.length) lines.push(cur.join(' '));
      cur = [];
    }
    if (p[key] != null) cur.push(`${x(t).toFixed(1)},${y(p[key]).toFixed(1)}`);
    prev = t;
  }
  if (cur.length) lines.push(cur.join(' '));
  return { lines, lo, hi, y };
}

// HISTORY_DAYS — периоды графика.
export const HISTORY_DAYS = [1, 7, 30, 90];
