// Shutdown and restart: shell/power.js, lib/Power.svelte.
export default {
  reboot: { label: 'Restart', q: 'Restart the server?', done: 'The server is restarting…' },
  poweroff: { label: 'Shut down', q: 'Shut down the server?', done: 'The server is shutting down. It can be turned on again only with the power button on the case or via power management.' },
  warn: 'Network copying (SMB), storage tasks and all web interface sessions will be interrupted.',
  aria: 'Power',
  title: 'Power: restart, shut down',
  sending: 'Sending…',
  reloadHint: 'The page will reload when the server responds again.',
};
