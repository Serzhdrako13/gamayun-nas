// App.svelte, Palette.svelte, api.js.
export default {
  theme: { auto: 'Тема: авто', light: 'Тема: светлая', dark: 'Тема: тёмная' },
  langButton: 'Язык: RU',
  stale: 'Данные не обновлены: {error}',
  updated: 'Обновлено {time}',
  unreadTitle: 'Непрочитанные уведомления',
  unread: 'Уведомления: {count}',
  search: 'Поиск Ctrl+K',
  logout: 'Выйти ({name})',
  palette: {
    aria: 'Поиск',
    placeholder: 'Раздел, пул, действие…',
    go: 'Перейти: {label}',
    createPool: 'Создать пул…',
    pool: 'Пул {name}',
  },
  api: {
    network: 'Нет связи с сервером',
    failed: 'Сбой запроса (код {status})',
  },
};
