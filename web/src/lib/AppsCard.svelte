<script>
  // Подсистема приложений (Docker) на пуле — вкл/выкл, состояние,
  // возобновление после паузы. API: /apps, /apps/enable|disable|resume.
  import { app, flash } from './state.svelte.js';
  import { post } from './api.js';
  import Switch from './Switch.svelte';
  import HelpButton from '../help/HelpButton.svelte';
  import { subKind } from './apps.js';
  import { t } from './i18n/index.js';

  let { v, onchange } = $props();
  let err = $state('');
  let pool = $state('');
  let askOff = $state(false);
  let busy = $state(false);
  $effect(() => {
    if (!pool) pool = v?.pool || app.pools[0]?.name || '';
  });

  async function act(path, body, msg) {
    err = '';
    busy = true;
    try {
      await post(path, body);
      flash(msg);
      onchange?.();
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }
</script>

<section class="card stack">
  <div class="row between">
    <h2>{t('apps.title')} <HelpButton section="apps" /></h2>
    {#if v && v.state !== 'disabled'}<span class="badge {subKind(v.state)}">{t('apps.state.' + v.state)}</span>{/if}
  </div>
  {#if err}<p class="error" role="alert">{err}</p>{/if}
  {#if v}
    {#if !v.installed}<p class="banner warn">{t('apps.notInstalled')}</p>{/if}
    {#if v.reason && v.enabled}<p class="banner {v.state === 'error' ? 'bad' : 'warn'}">{v.reason} <HelpButton section="apps.unavailable" /></p>{/if}
    {#if v.enabled}
      <Switch label={t('apps.title')} hint={t('apps.onPool', { pool: v.pool })} checked={true} disabled={busy} onchange={() => (askOff = true)} />
      {#if askOff}
        <div class="row">
          <span>{t('apps.askOff')}</span>
          <button class="danger" onclick={() => { askOff = false; act('/apps/disable', {}, t('apps.disabledFlash')); }}>{t('apps.confirmOff')}</button>
          <button onclick={() => (askOff = false)}>{t('common.cancel')}</button>
        </div>
      {/if}
      {#if v.state === 'paused' || v.state === 'error' || v.state === 'busy'}
        <div><button disabled={busy} onclick={() => act('/apps/resume', {}, t('apps.resumedFlash'))}>{t('apps.resume')}</button></div>
      {/if}
    {:else if v.installed && app.pools.length}
      <label class="field"><span>{t('common.pool')}</span><select bind:value={pool}>{#each app.pools as p}<option value={p.name}>{p.name}</option>{/each}</select></label>
      <Switch label={t('apps.title')} hint={t('apps.enableHint')} checked={false} disabled={busy || !pool} onchange={() => act('/apps/enable', { pool }, t('apps.enabledFlash', { pool }))} />
    {:else if v.installed}<p class="muted">{t('common.createPoolFirst')}</p>{/if}
  {:else if !err}<p class="muted">{t('common.loading')}</p>{/if}
</section>
