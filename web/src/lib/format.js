// Форматирование и разбор данных API для экранов.
import { t, locale } from './i18n/index.js';

const UNITS = ['b', 'kib', 'mib', 'gib', 'tib', 'pib']; // ключи format.units.*

export function bytes(n) {
  if (n == null || Number.isNaN(Number(n))) return '—';
  let v = Number(n);
  let i = 0;
  while (v >= 1024 && i < UNITS.length - 1) {
    v /= 1024;
    i++;
  }
  const s = i === 0 || v >= 100 ? v.toFixed(0) : v.toFixed(v >= 10 ? 1 : 2);
  return s.replace('.', t('format.decimal')) + ' ' + t('format.units.' + UNITS[i]);
}

export const POOL_TYPES = {
  get basic() {
    return t('format.poolType.basic');
  },
  jbod: 'JBOD',
  raid0: 'RAID 0',
  raid1: 'RAID 1',
  raid5: 'RAID 5',
  raid6: 'RAID 6',
  raid10: 'RAID 10',
  shr: 'SHR',
  shr2: 'SHR-2',
};

export function failures(n) {
  if (n === 0) return t('format.failures.none');
  // «1 отказ диска», иначе «N отказ/отказа/отказов дисков» (форма — pluralForm).
  if (n === 1) return t('format.failures.single');
  return t('format.failures.many', { count: n });
}

// Подписи — геттерами: текст читается на текущем языке при каждом обращении.
const healthOf = (k, cls) => [t('format.health.' + k), cls];
export const HEALTH = {
  get ok() { return healthOf('ok', 'ok'); },
  // Массивы исправны, Btrfs аварийно только для чтения.
  get warning() { return healthOf('warning', 'warn'); },
  get resyncing() { return healthOf('resyncing', 'warn'); },
  get reshaping() { return healthOf('reshaping', 'warn'); },
  get rebuilding() { return healthOf('rebuilding', 'warn'); },
  get degraded() { return healthOf('degraded', 'warn'); },
  get failed() { return healthOf('failed', 'bad'); },
  get missing() { return healthOf('missing', 'bad'); },
  // Опрос части стека не удался — состояние не определено.
  get unknown() { return healthOf('unknown', 'warn'); },
};

export function health(pool) {
  const h = pool.status && pool.status.health;
  if (!h) return [t('format.health.noData'), 'unknown'];
  return HEALTH[h] || [h, 'unknown'];
}

// Ход начальной синхронизации нового пула; null — не идёт.
export function syncEta(min) {
  const m = Math.max(1, Math.round(min));
  return m >= 60 ? t('format.sync.etaHours', { h: Math.floor(m / 60), m: m % 60 }) : t('format.sync.etaMinutes', { m });
}

export function syncText(pool) {
  const s = pool.status && pool.status.initial_sync && pool.status.sync;
  if (!s) return null;
  const stage = s.stage === 'integrity' ? t('format.sync.stageIntegrity') : t('format.sync.stageMirror');
  const eta = s.eta_min != null ? t('format.sync.left', { eta: syncEta(s.eta_min) }) : t('format.sync.noEta');
  return t('format.sync.text', { pct: Math.floor(s.percent), stage, eta });
}

// Баннер read-only: что показать по полю protection пула.
export function protectionBanner(pool) {
  const p = pool.protection;
  if (!p) return null;
  // Защита выключена администратором — баннер постоянный, пока не включат.
  if (p.enabled === false) {
    const off = t(p.redundancy_exhausted ? 'format.protection.offExhausted' : 'format.protection.off');
    if (pool.status?.emergency_read_only) return { kind: 'bad', text: t('format.protection.emergencyRo') + '. ' + off, override: false, off: true };
    return { kind: p.redundancy_exhausted ? 'bad' : 'warn', text: off, override: false, off: true };
  }
  if (p.override) return { kind: 'warn', text: t('format.protection.override', { time: new Date(p.override.expires_at).toLocaleString(locale()) }), override: true };
  // RO выставило ядро (ошибка записи), а не защита — своя причина.
  if (pool.status?.emergency_read_only && !p.write_blocked) return { kind: 'bad', text: t('format.protection.emergencyRo'), override: false };
  if (p.read_only || p.write_blocked) {
    return { kind: 'bad', text: p.reason || t('format.protection.readOnly'), override: false };
  }
  if (p.redundancy_exhausted) return { kind: 'warn', text: t('format.protection.exhausted'), override: false };
  return null;
}

// Диски, пригодные для пула: известная идентичность и роль unknown (не системные).
export function freeDisks(snapshot) {
  if (!snapshot || !snapshot.complete) return [];
  return (snapshot.disks || []).filter((d) => d.id && d.system_role === 'unknown' && d.size_bytes);
}

export function diskState(d) {
  if (d.system_role === 'unknown') return d.id ? [t('format.disk.free'), 'ok'] : [t('format.disk.noId'), 'warn'];
  const sys = () => [t('format.disk.system'), 'info'];
  return { root: sys, system: sys, in_use: () => [t('format.disk.inUse'), 'info'] }[d.system_role]?.() || [d.system_role, 'unknown'];
}

export function dateTime(s) {
  return s ? new Date(s).toLocaleString(locale()) : '—';
}
