// Виджет «Активность сети»: выбор адаптера, Мбит/с, график.
// Чистые функции — проверяются в lib/shell.test.js.

import { t, locale } from '../lib/i18n/index.js';

export const HISTORY = 36; // точек графика: 3 мин при опросе раз в 5 с

// mbps — байт/с в «12,3 Мбит/с» (точность по величине); null — «—».
export function mbps(v) {
  if (v == null || !isFinite(v)) return '—';
  const m = (v * 8) / 1e6;
  const d = m >= 100 ? 0 : m >= 10 ? 1 : 2;
  return m.toLocaleString(locale(), { minimumFractionDigits: d, maximumFractionDigits: d }) + ' ' + t('widgets.mbps');
}

// rates — скорость {rx, tx} (байт/с) выбранного адаптера; '' — все сразу.
// Нет данных или адаптера — null.
export function rates(m, iface) {
  if (!m) return { rx: null, tx: null };
  if (!iface) return { rx: m.net_rx_bytes_per_sec ?? null, tx: m.net_tx_bytes_per_sec ?? null };
  const i = (m.net_by_iface || []).find((x) => x.name === iface);
  return { rx: i?.rx_bytes_per_sec ?? null, tx: i?.tx_bytes_per_sec ?? null };
}

// push — новая точка в конец, не больше HISTORY.
export function push(hist, point) {
  const out = [...hist, point];
  return out.length > HISTORY ? out.slice(out.length - HISTORY) : out;
}

// line — атрибут points полилинии в рамке w×h; масштаб общий (max),
// пропуски (null) — ноль; точки прижаты вправо (новые — справа).
export function line(values, max, w, h) {
  if (!values.length) return '';
  const step = w / (HISTORY - 1);
  const off = w - step * (values.length - 1);
  const top = max > 0 ? max : 1;
  return values.map((v, i) => `${(off + i * step).toFixed(1)},${(h - ((v || 0) / top) * (h - 2) - 1).toFixed(1)}`).join(' ');
}
