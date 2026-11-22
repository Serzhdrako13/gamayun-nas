<script>
  import HelpButton from '../help/HelpButton.svelte';
  // Тишина уведомлений на время работ. Журнал и центр — как обычно,
  // почта и webhook молчат (кроме критичных, если не отмечено иное); по
  // окончании — одна сводка «за время тишины не отправлено N».
  import { onMount } from 'svelte';
  import { flash } from './state.svelte.js';
  import { get, put, del } from './api.js';
  import { QUIET_PRESETS, quietUntil, quietText } from './quiet.js';
  import { t, locale } from './i18n/index.js';

  let q = $state(null);
  let loaded = $state(false);
  let preset = $state('1h');
  let custom = $state('');
  let reason = $state('');
  let muteCritical = $state(false);
  let busy = $state(false);
  let err = $state('');

  async function load() {
    try {
      q = (await get('/notifications/quiet')).quiet;
      loaded = true;
    } catch (e) {
      err = e.message;
    }
  }
  onMount(load);

  const until = $derived(quietUntil(preset, new Date(), custom));

  async function start() {
    err = '';
    const u = quietUntil(preset, new Date(), custom);
    if (!u) return (err = t('quiet.errTerm'));
    if (!reason.trim()) return (err = t('quiet.errReason'));
    busy = true;
    try {
      q = (await put('/notifications/quiet', { until: u.toISOString(), reason, mute_critical: muteCritical })).quiet;
      flash(t('quiet.startedFlash', { time: new Date(q.until).toLocaleString(locale()) }));
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }
  async function stop() {
    err = '';
    busy = true;
    try {
      await del('/notifications/quiet');
      await load();
      flash(t('quiet.stoppedFlash'));
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }
</script>

{#if loaded}
  <section class="card stack">
    <div class="row between">
      <h2>{t('quiet.title')} <HelpButton section="notify" /></h2>
      {#if q}<span class="badge warn">{t('quiet.on')}</span>{/if}
    </div>
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <p class="small {q ? '' : 'muted'}">{quietText(q)}</p>
    {#if q?.ending}<p class="small muted">{t('quiet.ending')}</p>{/if}
    <div class="row">
      {#each QUIET_PRESETS as [k, label]}
        <label class="row"><input type="radio" name="quiet-preset" value={k} bind:group={preset} /> {label}</label>
      {/each}
    </div>
    {#if preset === 'custom'}
      <label class="field"><span>{t('quiet.customLabel')}</span><input type="datetime-local" bind:value={custom} /></label>
    {/if}
    <label class="field"><span>{t('quiet.reason')}</span><input bind:value={reason} maxlength="200" placeholder={t('quiet.reasonPlaceholder')} /></label>
    <label class="row"><input type="checkbox" bind:checked={muteCritical} /> {t('quiet.muteCritical')}</label>
    {#if muteCritical}
      <p class="banner warn">{t('quiet.muteCriticalWarn')}</p>
    {/if}
    <div class="row">
      <button class="primary" disabled={busy || !until || q?.ending} onclick={start}>{q ? t('quiet.change') : t('quiet.start')}</button>
      {#if q}<button disabled={busy || q.ending} onclick={stop}>{t('quiet.stop')}</button>{/if}
      {#if until}<small class="muted">{t('quiet.untilShort', { time: until.toLocaleString(locale()) })}</small>{/if}
    </div>
  </section>
{/if}
