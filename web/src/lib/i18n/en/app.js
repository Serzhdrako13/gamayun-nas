// App.svelte, Palette.svelte, api.js.
export default {
  theme: { auto: 'Theme: auto', light: 'Theme: light', dark: 'Theme: dark' },
  langButton: 'Language: EN',
  stale: 'Data not updated: {error}',
  updated: 'Updated {time}',
  unreadTitle: 'Unread notifications',
  unread: 'Notifications: {count}',
  search: 'Search Ctrl+K',
  logout: 'Sign out ({name})',
  palette: {
    aria: 'Search',
    placeholder: 'Section, pool, action…',
    go: 'Go to: {label}',
    createPool: 'Create pool…',
    pool: 'Pool {name}',
  },
  api: {
    network: 'No connection to the server',
    failed: 'Request failed (code {status})',
  },
};
