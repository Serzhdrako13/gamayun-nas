<script>
  // Приложение из каталога — версия, обновление (проверка →
  // подтверждение рисков → задача со снимком и автооткатом), ручной откат.
  import { untrack, onMount } from 'svelte';
  import { flash } from './state.svelte.js';
  import { get, post } from './api.js';
  import CheckReport from './CheckReport.svelte';
  import Confirm from './Confirm.svelte';
  import HelpButton from '../help/HelpButton.svelte';
  import { ready, waitTask } from './apps.js';
  import { textOf } from './catalog.js';
  import { dateTime } from './format.js';
  import { t, lang } from './i18n/index.js';

  let { d, running, onchange, autocheck = false } = $props();
  const id = untrack(() => d.id);
  const enc = encodeURIComponent(id);
  let upd = $state(null); // итог проверки обновления
  let notes = $state(null); // манифест новой версии (примечания)
  let updConfirm = $state(new Set());
  let err = $state('');
  let info = $state('');
  let busy = $state(false);
  const prev = $derived(d.catalog?.points?.[0]);

  function toggle(set0, key, on) {
    const s = new Set(set0);
    on ? s.add(key) : s.delete(key);
    return s;
  }

  async function run(fn, done) {
    err = '';
    info = '';
    busy = true;
    try {
      const tk = await waitTask(get, await fn());
      if (tk.state === 'succeeded') {
        flash(done);
        upd = null;
        onchange();
      } else if (tk.state === 'failed') {
        err = t('apps.taskFailed', { error: tk.error?.message || '' });
        onchange();
      } else flash(t('apps.taskRunning'));
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }

  async function checkUpdate() {
    err = '';
    info = '';
    busy = true;
    upd = null;
    try {
      upd = await post(`/apps/projects/${enc}/catalog-update/check`, { confirm: [...updConfirm] });
      try {
        notes = (await get('/apps/catalog')).apps.find((e) => e.manifest.id === d.catalog.app)?.manifest || null;
      } catch {
        notes = null;
      }
    } catch (e) {
      if (e.status === 409 && !e.message.includes('Docker')) info = e.message || t('apps.catalog.noUpdate');
      else err = e.message;
    }
    busy = false;
  }

  // Кнопка «Обновить» в разделе «Каталог» открывает карточку сразу с проверкой обновления.
  let box;
  onMount(() => {
    if (autocheck && running) checkUpdate().then(() => box?.scrollIntoView({ block: 'nearest' }));
  });

</script>

<h3 bind:this={box}>{t('apps.catalog.badge')} <HelpButton section="apps.catalog" /></h3>
<p class="small">{t('apps.catalog.from', { version: d.catalog.version, app: d.catalog.app_version })}</p>
{#if d.catalog.points?.length}
  <p class="small muted">{t('apps.catalog.points', { list: d.catalog.points.map((p) => `${p.version} (${dateTime(p.at)})`).join(', ') })}</p>
{/if}
{#if err}<p class="error" role="alert">{err}</p>{/if}
{#if info}<p class="small" role="status">{info}</p>{/if}
<div class="row">
  <button disabled={!running || busy} onclick={checkUpdate}>{t('apps.catalog.checkUpdate')}</button>
  {#if prev}
    <Confirm label={t('apps.catalog.rollback', { version: prev.version })} hint={t('apps.catalog.rollbackHint')} disabled={!running || busy}
      onconfirm={() => run(() => post(`/apps/projects/${enc}/catalog-rollback`), t('apps.catalog.rolledBack', { id }))} />
  {/if}
</div>

{#if upd}
  <div class="card stack">
    <b>{t('apps.catalog.updateTitle', { from: upd.from, to: upd.to })}</b>
    {#if notes?.update?.security}<span class="badge warn">{t('apps.catalog.security')}</span>{/if}
    {#if notes && textOf(notes.update?.release_notes, lang())}
      <p class="small"><b>{t('apps.catalog.notes')}:</b> {textOf(notes.update.release_notes, lang())}</p>
    {/if}
    {#if upd.migration_irreversible}<p class="banner warn">{t('apps.catalog.irreversible')}</p>{/if}
    <CheckReport check={upd} confirm={updConfirm} ontoggle={(k, on) => (updConfirm = toggle(updConfirm, k, on))} />
    <div class="row">
      <button class="primary" disabled={!running || busy || !ready(upd, updConfirm)}
        onclick={() => run(() => post(`/apps/projects/${enc}/catalog-update`, { confirm: [...updConfirm] }), t('apps.catalog.updated', { id }))}>
        {t('apps.catalog.update')}</button>
      <button onclick={() => (upd = null)}>{t('common.cancel')}</button>
    </div>
  </div>
{/if}
