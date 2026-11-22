// Подписи событий журнала для виджета «События».
import { t } from '../lib/i18n/index.js';

// Тексты — словарь events; данные события (имена, причины) — как есть.
const withUser = (d) => (d.user ? ' ' + d.user : '');
const LABEL = {
  'auth.login': (d) => t(d.result === 'failure' ? 'events.loginFailed' : 'events.login') + withUser(d) + (d.remote ? t('events.from', { host: d.remote.replace(/:\d+$/, '') }) : ''),
  'auth.logout': (d) => t('events.logout', { user: d.user || '' }),
  'auth.lockout': (d) => t('events.lockout') + withUser(d),
  'auth.password_changed': (d) => t('events.passwordChanged', { user: d.user || '' }),
  'auth.user_created': (d) => t('events.userCreated', { user: d.user || '' }),
  'auth.setup': () => t('events.setup'),
  'auth.unlock': (d) => t('events.unlock') + withUser(d) + (d.remote ? t('events.from', { host: d.remote }) : ''),
  'auth.app_login': (d) => t('events.appLogin', { user: d.user || '', host: d.host || '' }),
  'config.backup.saved': (d) => t('events.backupSaved', { where: d.pool || d.sftp || d.usb || '' }),
  'config.backup.failed': (d) => t('events.backupFailed', { where: d.pool || d.sftp || d.usb || '' }),
  'system.reboot': (d) => t('events.reboot') + (d.user ? ' (' + d.user + ')' : ''),
  'system.ssh.enabled': (d) => t('events.sshOn') + (d.user ? ' (' + d.user + ')' : ''),
  'system.ssh.disabled': (d) => t(d.kill_sessions ? 'events.sshOffKill' : 'events.sshOff') + (d.user ? ' (' + d.user + ')' : ''),
  'system.poweroff': (d) => t('events.poweroff') + (d.user ? ' (' + d.user + ')' : ''),
  'notify.quiet_started': (d) => t(d.changed ? 'events.quietChanged' : 'events.quietStarted') + (d.reason ? ': ' + d.reason : ''),
  'notify.quiet_ended': (d) => t('events.quietEnded') + (d.suppressed ? t('events.quietEndedSuppressed', { count: d.suppressed }) : ''),
  // Снапшоты общих папок
  'snapshot.created': (d) => t('events.snapshot.created', { share: d.share || '' }),
  'snapshot.deleted': (d) => t('events.snapshot.deleted', { share: d.share || '' }),
  'snapshot.restored': (d) => t('events.snapshot.restored', { share: d.share || '', path: d.restored_as || d.path || '' }),
  'snapshot.restore_failed': (d) => t('events.snapshot.restoreFailed', { share: d.share || '', path: d.path || '' }),
  'snapshot.skipped': (d) => t('events.snapshot.skipped', { share: d.share || '', reason: d.reason === 'low_space' ? t('events.snapshot.lowSpace') : d.reason === 'read_only' ? t('events.snapshot.readOnly') : d.reason || '' }),
  'snapshot.failed': (d) => (d.step ? t('events.snapshot.failedStep', { share: d.share || '', step: d.step }) : t('events.snapshot.failed', { share: d.share || '' })),
  // Приложения Docker
  'apps.enabled': (d) => t('events.apps.enabled', { pool: d.pool || '' }),
  'apps.disabled': () => t('events.apps.disabled'),
  'apps.started': () => t('events.apps.started'),
  'apps.start_failed': (d) => t('events.apps.startFailed') + (d.reason ? ': ' + d.reason : ''),
  'apps.paused': () => t('events.apps.paused'),
  'apps.unavailable': (d) => t('events.apps.unavailable', { pool: d.pool || '' }),
  'apps.pool_busy': (d) => t('events.apps.poolBusy', { pool: d.pool || '' }),
  'apps.resumed': () => t('events.apps.resumed'),
  'apps.container_died': (d) => t('events.apps.containerDied', { name: d.container || '', code: d.exit_code || '' }) + appsRepeats(d),
  'apps.container_oom': (d) => t('events.apps.containerOom', { name: d.container || '' }) + appsRepeats(d),
  'apps.container_unhealthy': (d) => t('events.apps.containerUnhealthy', { name: d.container || '' }) + appsRepeats(d),
  'apps.container_healthy': (d) => t('events.apps.containerHealthy', { name: d.container || '' }),
  'apps.container_restart': (d) => t('events.apps.containerRestart', { name: d.container || '' }) + appsRepeats(d),
  'apps.app_created': (d) => t('events.apps.appCreated', { app: d.app || '', actor: d.actor || '' }),
  'apps.app_updated': (d) => t('events.apps.appUpdated', { app: d.app || '', actor: d.actor || '' }),
  'apps.app_deleted': (d) => t(d.keep_data ? 'events.apps.appDeletedKeep' : 'events.apps.appDeleted', { app: d.app || '', actor: d.actor || '' }),
  'apps.app_failed': (d) => t('events.apps.appFailed', { app: d.app || '', stage: d.stage || '' }) + (d.reason ? ': ' + d.reason : ''),
  'apps.app_crashing': (d) => t('events.apps.appCrashing', { app: d.app || '' }),
  'apps.app_started': (d) => t('events.apps.appStarted', { app: d.app || '', actor: d.actor || '' }),
  'apps.app_stopped': (d) => t('events.apps.appStopped', { app: d.app || '', actor: d.actor || '' }),
  'apps.risk_confirmed': (d) => t('events.apps.riskConfirmed', { app: d.app || '', actor: d.actor || '', risks: (d.risks || []).map((r) => r.key).join(', ') }),
};
function appsRepeats(d) { return d.suppressed ? t('events.apps.suppressed', { count: d.suppressed }) : ''; }

// Уведомления: события оборудования. Подписи — геттеры.
const titles = (map) => Object.defineProperties({}, Object.fromEntries(Object.entries(map).map(([k, key]) => [k, { get: () => t(key), enumerable: true }])));
export const NOTIFY_TITLE = titles({
  'disk.added': 'events.notify.diskAdded',
  'disk.removed': 'events.notify.diskRemoved',
  'disk.returned': 'events.notify.diskReturned',
  'disk.failed': 'events.notify.diskFailed',
  'array.degraded': 'events.notify.arrayDegraded',
  'array.recovering': 'events.notify.arrayRecovering',
  'array.recovered': 'events.notify.arrayRecovered',
  'integrity.errors': 'events.notify.integrityErrors',
  'pool.emergency_ro': 'events.notify.poolEmergencyRo', 'pool.emergency_ro_cleared': 'events.notify.poolEmergencyRoCleared',
  'pool.protection_bypassed': 'events.notify.poolProtectionBypassed', 'pool.protection_bypassed_cleared': 'events.notify.poolProtectionBypassedCleared',
  'array.readd_failed': 'events.notify.arrayReaddFailed',
  'disk.overheat': 'events.notify.diskOverheat', 'disk.temp_normal': 'events.notify.diskTempNormal',
});
const sev = (k, kind) => ({ get 0() { return t('events.severity.' + k); }, 1: kind });
export const SEVERITY = { info: sev('info', 'ok'), warning: sev('warning', 'warn'), critical: sev('critical', 'bad') };

export function notifyText(n) {
  const s = NOTIFY_TITLE[n.type] || LABEL[n.type]?.(n.data || {}) || n.type;
  return n.subject ? s + ' · ' + n.subject.replace(/^(disk|array|pool|container|app):/, (m, k) => t('events.subject.' + k)) : s;
}

export function eventText(ev) {
  const f = LABEL[ev.type];
  let d = {};
  try { d = typeof ev.data === 'string' ? JSON.parse(ev.data) : ev.data || {}; } catch { /* нет данных */ }
  return f ? f(d).trim() : NOTIFY_TITLE[ev.type] || ev.type;
}

export function eventKind(ev) {
  if (ev.severity === 'warning' || ev.severity === 'critical') return 'warn';
  return /failure|lockout|error|failed/.test(ev.type + JSON.stringify(ev.data || '')) ? 'warn' : 'ok';
}
