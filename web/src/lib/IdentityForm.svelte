<script>
  import { onMount } from 'svelte';
  import { get, put } from './api.js';
  import { flash } from './state.svelte.js';
  import Switch from './Switch.svelte';
  import { t } from './i18n/index.js';

  // Имя сервера, часовой пояс и NTP — общая форма мастера и настроек.
  let { submitLabel, onsaved = () => {} } = $props(); // без submitLabel — «Сохранить» на текущем языке
  let hostname = $state('');
  let timezone = $state('');
  let ntp = $state(true);
  let zones = $state([]);
  let synced = $state(null);
  let err = $state('');
  let warns = $state([]);
  let busy = $state(false);
  let loaded = $state(false);

  onMount(async () => {
    try {
      const r = await get('/system/identity');
      hostname = r.hostname;
      timezone = r.timezone;
      ntp = r.ntp;
      synced = r.ntp_synced;
      zones = r.timezones || [];
      loaded = true;
    } catch (e) {
      err = e.message;
    }
  });
  const hostOk = $derived(/^[a-z]([a-z0-9-]{0,13}[a-z0-9])?$/.test(hostname));
  const zoneOk = $derived(zones.includes(timezone));

  async function save(e) {
    e.preventDefault();
    busy = true;
    err = '';
    warns = [];
    try {
      const r = await put('/system/identity', { hostname, timezone, ntp });
      synced = r.identity.ntp_synced;
      warns = r.warnings || [];
      flash(t('identity.saved'));
      onsaved(r);
    } catch (x) {
      err = x.message;
    } finally {
      busy = false;
    }
  }
</script>

<form onsubmit={save} class="stack">
  <label class="field">
    <span>{t('identity.hostname')}</span>
    <input bind:value={hostname} autocomplete="off" required />
  </label>
  {#if hostname && !hostOk}<p class="error">{t('identity.hostnameBad')}</p>{/if}
  <label class="field">
    <span>{t('identity.timezone')}</span>
    <input list="dn-zones" bind:value={timezone} autocomplete="off" required />
    <datalist id="dn-zones">{#each zones as z}<option value={z}></option>{/each}</datalist>
  </label>
  {#if timezone && loaded && !zoneOk}<p class="error">{t('identity.timezoneBad')}</p>{/if}
  <Switch label={t('identity.ntp')} hint={synced === null ? '' : t('identity.ntpNow', { state: synced ? t('identity.synced') : t('identity.notSynced') })} checked={ntp} onchange={(v) => (ntp = v)} />
  {#if err}<p class="error" role="alert">{err}</p>{/if}
  {#each warns as w}<p class="banner warn">{w}</p>{/each}
  <button class="primary" disabled={busy || !loaded || !hostOk || !zoneOk}>{submitLabel ?? t('common.save')}</button>
</form>
