// Server name, time zone, NTP: lib/IdentityForm.svelte.
export default {
  saved: 'Server name and time saved',
  hostname: 'Server name (lowercase Latin letters, digits and hyphen, up to 15 characters) — how it appears on the network and over SMB',
  hostnameBad: 'Invalid name: up to 15 characters, lowercase Latin letters, digits and hyphen, starting with a letter.',
  timezone: 'Time zone',
  timezoneBad: 'This time zone is not in the list (for example, Europe/Moscow).',
  ntp: 'Synchronize time over the network (NTP)',
  ntpNow: 'currently {state}',
  synced: 'synchronized',
  notSynced: 'not synchronized',
};
