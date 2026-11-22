// Снапшоты общей папки — подписи и разбор для экрана.
import { t } from './i18n/index.js';

const pad = (n) => String(n).padStart(2, '0');

// Время снапшота (ISO из API) в местном времени браузера: «18.11.2026 10:00».
export function snapTime(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '—';
  return `${pad(d.getDate())}.${pad(d.getMonth() + 1)}.${d.getFullYear()} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// Родительский путь и цепочка «хлебных крошек» для просмотра снапшота.
export function parentPath(p) {
  return p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '';
}

export function crumbs(p) {
  const out = [{ name: t('snapshots.root'), path: '' }];
  if (!p) return out;
  let cur = '';
  for (const c of p.split('/')) {
    cur = cur ? cur + '/' + c : c;
    out.push({ name: c, path: cur });
  }
  return out;
}

export const joinPath = (dir, name) => (dir ? dir + '/' + name : name);

// Уровни хранения расписания: поле, подпись, предел (как в gamayund).
// Подпись [1] — геттер: читается на текущем языке при каждом обращении.
const level = (k, max) => {
  const a = [k, '', max];
  Object.defineProperty(a, 1, { get: () => t('snapshots.levels.' + k), enumerable: true });
  return a;
};
export const LEVELS = [level('hourly', 336), level('daily', 366), level('weekly', 260), level('monthly', 120)];

// Ошибка расписания до отправки (дублирует проверку gamayund) или ''.
export function policyError(p) {
  let sum = 0;
  for (const [k, label, max] of LEVELS) {
    const v = Number(p[k]);
    if (!Number.isInteger(v) || v < 0 || v > max) return t('snapshots.levelError', { label, max });
    sum += v;
  }
  if (p.enabled && sum === 0) return t('snapshots.noLevel');
  return '';
}

// Краткая сводка расписания: «24 ч · 7 дн · 4 нед · 6 мес».
export function policySummary(p) {
  if (!p || !p.enabled) return t('snapshots.summaryOff');
  const parts = [];
  for (const k of ['hourly', 'daily', 'weekly', 'monthly']) if (p[k]) parts.push(t('snapshots.unit.' + k, { n: p[k] }));
  return t('snapshots.summaryKeep', { parts: parts.join(' · ') });
}

// Итог восстановления для сообщения.
export function restoreText(r) {
  if (!r) return t('snapshots.restored');
  let s = t('snapshots.restoredAs', { path: r.path });
  if (r.to_root) s += t('snapshots.restoredToRoot');
  if (r.skipped) s += t('snapshots.restoredSkipped', { count: r.skipped });
  return s;
}
