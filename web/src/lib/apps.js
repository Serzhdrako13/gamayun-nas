// Помощники раздела «Приложения» (без Svelte — тестируются в node).

// Цвет значка состояния приложения.
const KIND = {
  running: 'ok', running_no_check: 'ok', starting: 'warn', unhealthy: 'warn', paused: 'warn', waiting_pool: 'warn',
  stopped: '', removed: '', crashing: 'bad', error: 'bad', unknown: '',
};
export const stateKind = (s) => KIND[s] ?? '';

// Цвет значка подсистемы.
const SUB = { running: 'ok', starting: 'warn', paused: 'warn', busy: 'warn', unavailable: 'warn', not_installed: 'warn', error: 'bad' };
export const subKind = (s) => SUB[s] ?? '';

export const portKey = (p) => `${p.host}/${p.proto}`;

// Ссылка на порт приложения: только TCP; «только на NAS» — без ссылки (127.0.0.1 недоступен из браузера).
export function portUrl(p, host) {
  if (p.proto !== 'tcp' || !p.network || !host) return '';
  const scheme = [443, 8443].includes(p.container) ? 'https' : 'http';
  return `${scheme}://${host}:${p.host}/`;
}

// Готовность к созданию: проверка без запретов и конфликтов, все риски отмечены.
export function ready(check, confirm) {
  if (!check || check.forbidden?.length || check.conflicts?.length) return false;
  const ok = new Set(confirm || []);
  return (check.unconfirmed || []).every((k) => ok.has(k));
}

// Тело запроса создания/изменения. env: null — оставить текущие (изменение).
// run_as — выбранные режимы запуска, grants — доступ к общим папкам.
export function requestBody(f, check, confirm, network, access = null) {
  const b = {
    id: f.id,
    name: f.name,
    compose: f.compose,
    env: f.keepEnv ? null : f.env,
    network: [...network].sort(),
    confirm: (check?.risks || []).map((r) => r.key).filter((k) => confirm.has(k)),
  };
  if (access) {
    b.run_as = { ...access.runAs };
    b.grants = { ...access.grants };
    if (access.migrate) b.migrate = true;
  }
  return b;
}

// Общие папки из bind compose (проверка), по одной строке на папку.
export function shareRows(check) {
  const m = new Map();
  for (const b of check?.share_binds || []) {
    const r = m.get(b.share) || { share: b.share, services: [], readOnly: true };
    if (!r.services.includes(b.service)) r.services.push(b.service);
    r.readOnly = r.readOnly && b.read_only;
    m.set(b.share, r);
  }
  return [...m.values()].sort((a, b) => a.share.localeCompare(b.share));
}

// Выдачи по умолчанию для папок без выдачи: «только чтение»; при переводе
// приложения «доступ как root» — как было (без :ro — чтение и запись: оно уже писало).
export function fillGrants(grants, check, migrate = false) {
  const g = { ...grants };
  let added = false;
  for (const r of shareRows(check)) {
    if (!g[r.share]) {
      g[r.share] = migrate && !r.readOnly ? 'rw' : 'ro';
      added = true;
    }
  }
  return { grants: g, added };
}

// Только выдачи для папок, которые есть в compose (лишние не отправляем).
export function usedGrants(grants, check) {
  const used = new Set(shareRows(check).map((r) => r.share));
  return Object.fromEntries(Object.entries(grants).filter(([k]) => used.has(k)));
}

export const validId = (s) => /^[a-z][a-z0-9-]{0,31}$/.test(s) && !s.endsWith('-');

// Ожидание задачи gamayund: опрос раз в секунду до limitMs.
export async function waitTask(get, task, limitMs = 20 * 60 * 1000, sleep = (ms) => new Promise((r) => setTimeout(r, ms))) {
  const until = Date.now() + limitMs;
  let tk = task;
  while ((tk.state === 'queued' || tk.state === 'running') && Date.now() < until) {
    await sleep(1000);
    tk = await get(`/tasks/${encodeURIComponent(tk.id)}`);
  }
  return tk;
}
