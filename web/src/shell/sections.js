// Реестр разделов. Новый раздел = манифест здесь + содержимое
// (views.js): плитка на главной, пункт меню, ярлык, вкладка и окно появятся
// сами в обоих стилях. Без импорта .svelte — проверяется тестом.
//   id     — стабильный ключ (вкладки, положение окон пользователя)
//   title  — название раздела; icon — имя из lib/Icon.svelte
//   nav    — внутреннее меню окна: [{label, view}], view — ключ в views.js
//   desc   — подпись под плиткой; shortcut — ярлык на рабочем столе по умолчанию
//   soon   — раздел виден, но ещё не реализован (шаг карты)
import { t } from '../lib/i18n/index.js';

// Подписи — геттеры по словарю sections: читают текущий язык.
const sec = (id, icon, nav, extra = {}) => ({
  id,
  get title() { return t(`sections.${id}.title`); },
  icon,
  get desc() { return t(`sections.${id}.desc`); },
  ...extra,
  nav: nav.map(([key, view]) => ({ get label() { return t(`sections.${id}.${key}`); }, view })),
});

export const SECTIONS = [
  sec('storage', 'store', [['pools', 'pools'], ['disks', 'disks']], { shortcut: true }),
  sec('shares', 'folder', [['folders', 'shares']], { shortcut: true }),
  sec('files', 'folder', [['files', 'files']], { shortcut: true }),
  sec('users', 'users', [['users', 'users']], { shortcut: true }),
  sec('journal', 'log', [['events', 'journal']], { shortcut: true }),
  sec('notify', 'bell', [['center', 'notifications'], ['channels', 'channels']], { shortcut: true }),
  sec('settings', 'set', [['server', 'settings'], ['confbackup', 'confbackup']], { shortcut: true }),
  sec('apps', 'apps', [['apps', 'apps'], ['catalog', 'appsCatalog'], ['ports', 'appsPorts']], { shortcut: true }),
  sec('help', 'help', [['contents', 'help']]),
  sec('backup', 'log', [], { soon: true }),
];

export const section = (id) => SECTIONS.find((s) => s.id === id);
