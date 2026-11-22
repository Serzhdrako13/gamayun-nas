// Логика мастера восстановления настроек (без DOM — проверяется тестом).
// По умолчанию — новые пароли и секреты; «оставить прежний» доступен только
// там, где он есть на этой системе (can_keep), и требует явного согласия.
import { t } from './i18n/index.js';

export const MIN_PASSWORD = 8;

// initChoices — начальные ответы: всё «задать новое».
export function initChoices(plan) {
  const users = {};
  for (const u of plan.users || []) users[u.name] = { mode: 'new', pw: '', pw2: '' };
  const secrets = {};
  for (const s of plan.secrets || []) secrets[s.key] = { mode: 'new', value: '' };
  return { users, secrets, acceptInsecure: false };
}

// anyKeep — выбрано хотя бы одно «оставить прежний».
export function anyKeep(ch) {
  return [...Object.values(ch.users), ...Object.values(ch.secrets)].some((x) => x.mode === 'keep');
}

// buildAnswers — тело answers для POST /config/restore и список ошибок формы.
export function buildAnswers(plan, ch) {
  const errors = [];
  const answers = { passwords: {}, keep: {}, accept_insecure: !!ch.acceptInsecure, smtp_password: '', webhook_secret: '', webhook_url: '' };
  for (const u of plan.users || []) {
    const c = ch.users[u.name] || { mode: 'new', pw: '', pw2: '' };
    if (c.mode === 'keep') {
      if (!u.can_keep) errors.push(t('confbackup.err.noOldPassword', { name: u.name }));
      else answers.keep[u.name] = true;
      continue;
    }
    if ((c.pw || '').length < MIN_PASSWORD) errors.push(t('confbackup.err.passwordShort', { name: u.name, min: MIN_PASSWORD }));
    else if (c.pw !== c.pw2) errors.push(t('confbackup.err.passwordMismatch', { name: u.name }));
    else answers.passwords[u.name] = c.pw;
  }
  for (const s of plan.secrets || []) {
    const c = ch.secrets[s.key] || { mode: 'new', value: '' };
    if (c.mode === 'keep') {
      if (!s.can_keep) errors.push(t('confbackup.err.noOldValue', { title: s.title }));
      else answers.keep[s.key] = true;
      continue;
    }
    answers[s.key] = c.value || '';
  }
  if (anyKeep(ch) && !ch.acceptInsecure) errors.push(t('confbackup.err.confirmInsecure'));
  return { answers, errors };
}

// secretsLeftEmpty — секреты без значения: канал будет выключен.
export function secretsLeftEmpty(plan, ch) {
  return (plan.secrets || []).filter((s) => (ch.secrets[s.key]?.mode ?? 'new') === 'new' && !ch.secrets[s.key]?.value).map((s) => s.title);
}

// toBase64 — ArrayBuffer → base64 (для поля file).
export function toBase64(buf) {
  const b = new Uint8Array(buf);
  let s = '';
  for (let i = 0; i < b.length; i += 0x8000) s += String.fromCharCode(...b.subarray(i, i + 0x8000));
  return btoa(s);
}

// Подписи действий — геттерами: читаются на текущем языке.
export const ACTIONS = {
  get create() { return t('confbackup.action.create'); },
  get update() { return t('confbackup.action.update'); },
  get skip() { return t('confbackup.action.skip'); },
};
