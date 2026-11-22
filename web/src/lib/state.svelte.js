// Общее состояние интерфейса: сессия, пулы, задачи; обновление — опросом
// (отдельный коллектор не нужен). Опрос идёт только при видимой вкладке.
import { get, setCsrf, setUnauthorizedHandler } from './api.js';
import { bindLangState, detectLang, normLang, DEFAULT_LANG } from './i18n/index.js';

export const app = $state({
  ready: false,
  session: null, // {user, csrf_token, ...}
  setupRequired: false,
  setup: null, // {done, product} — мастер первоначальной настройки
  product: 'GamayunNas', // имя продукта приходит от сервера (internal/brand)
  pools: [],
  poolsError: '',
  tasks: [],
  updatedAt: null,
  theme: 'dark', // тёмная — основная; светлая — заготовка; «авто» пока = тёмная
  lang: DEFAULT_LANG, // язык интерфейса: ru / en; t() читает его отсюда — смена перерисовывает разметку
  shell: 'tabs', // стиль оболочки: tabs — «Вкладки» (по умолчанию), desktop — «Рабочий стол»
  toast: '',
  unread: 0, // непрочитанные уведомления
});

bindLangState(app);

export function flash(msg) {
  app.toast = msg;
  setTimeout(() => {
    if (app.toast === msg) app.toast = '';
  }, 5000);
}

export async function loadSession() {
  try {
    app.session = await get('/auth/session');
    setCsrf(app.session.csrf_token);
    await loadSetup();
    app.setupRequired = false;
  } catch (e) {
    app.session = null;
    app.setupRequired = e.code === 'setup_required';
  }
  app.ready = true;
}

export async function loadSetup() {
  try {
    const r = await get('/setup/status');
    app.product = r.product || app.product;
    app.setup = r;
  } catch {
    app.setup = null;
  }
}

export async function setSession(s) {
  setCsrf(s.csrf_token);
  await loadSetup(); // до показа интерфейса: нужно знать, пройден ли мастер
  app.session = s;
}

setUnauthorizedHandler(() => {
  app.session = null;
  stopPolling();
});

export async function refresh() {
  try {
    const [p, t] = await Promise.all([get('/storage/pools'), get('/tasks?limit=20')]);
    app.pools = p.pools || [];
    app.tasks = t.tasks || [];
    app.poolsError = '';
    app.updatedAt = new Date();
  } catch (e) {
    app.poolsError = e.message;
  }
  try {
    app.unread = (await get('/notifications?limit=1&unread=1')).unread || 0;
  } catch {
    /* уведомления недоступны — счётчик не показываем */
  }
}

let timer = null;
const PERIOD = 5000;

function tick() {
  if (document.visibilityState === 'visible') refresh();
}

export function startPolling() {
  if (timer) return;
  refresh();
  timer = setInterval(tick, PERIOD);
  document.addEventListener('visibilitychange', tick);
}

export function stopPolling() {
  if (timer) clearInterval(timer);
  timer = null;
  document.removeEventListener('visibilitychange', tick);
}

// Тема: «авто» (системная) / светлая / тёмная; выбор помнится в браузере.
export function initTheme() {
  try {
    app.theme = localStorage.getItem('dn_theme') || 'dark';
    app.shell = localStorage.getItem('dn.shell') === 'desktop' ? 'desktop' : 'tabs';
  } catch {
    /* хранилище недоступно */
  }
  applyTheme();
}

// Стиль оболочки — в браузере (на сервер — идея, IDEAS 2026-11-06).
export function setShell(v) {
  app.shell = v === 'desktop' ? 'desktop' : 'tabs';
  try {
    localStorage.setItem('dn.shell', app.shell);
  } catch {
    /* приватный режим */
  }
}

export function setTheme(t) {
  app.theme = t;
  try {
    localStorage.setItem('dn_theme', t);
  } catch {
    /* ignore */
  }
  applyTheme();
}

function applyTheme() {
  const r = document.documentElement;
  if (app.theme === 'auto') r.removeAttribute('data-theme');
  else r.setAttribute('data-theme', app.theme);
}

// Язык интерфейса: выбор помнится в браузере (dn.lang); без выбора —
// язык браузера, если ru/en, иначе ru. В профиле пользователя на сервере — позже.
export function initLang() {
  let l = '';
  try {
    l = normLang(localStorage.getItem('dn.lang'));
  } catch {
    /* хранилище недоступно */
  }
  if (!l && typeof navigator !== 'undefined') l = detectLang(navigator.languages?.length ? navigator.languages : [navigator.language]);
  app.lang = l || DEFAULT_LANG;
  applyLang();
}

export function setLang(l) {
  app.lang = normLang(l) || DEFAULT_LANG;
  try {
    localStorage.setItem('dn.lang', app.lang);
  } catch {
    /* приватный режим */
  }
  applyLang();
}

function applyLang() {
  document.documentElement.lang = app.lang;
}
