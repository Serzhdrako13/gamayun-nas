<script>
  // Раздел «Каталог» — состояние подписанного каталога
  // (номер, ключ, источник), обновление по сети и импорт файлом, карточки
  // приложений с установкой; установленные — с отметкой версии/обновления.
  import { onMount } from 'svelte';
  import { flash } from '../lib/state.svelte.js';
  import { get, post, csrfToken, ApiError } from '../lib/api.js';
  import CatalogInstall from '../lib/CatalogInstall.svelte';
  import AppDetail from '../lib/AppDetail.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import HelpButton from '../help/HelpButton.svelte';
  import { dateTime } from '../lib/format.js';
  import { textOf, installedOf, hasUpdate } from '../lib/catalog.js';
  import { t, lang } from '../lib/i18n/index.js';

  let cat = $state(null);
  let apps = $state(null); // GET /apps/projects
  let err = $state('');
  let unavailable = $state(false);
  let busy = $state(false);
  let older = $state(null); // архив старее текущего — ждёт подтверждения
  let install = $state(null);
  let detail = $state('');
  let updating = $state(false); // открыта кнопкой «Обновить» — сразу проверка обновления // открытое установленное приложение (обновление, откат — в его карточке)
  let fileInput;
  const installed = $derived(installedOf(apps?.projects));
  const running = $derived(apps?.apps?.state === 'running');

  async function load() {
    try {
      cat = await get('/apps/catalog');
      unavailable = false;
    } catch (e) {
      if (e.status === 503) unavailable = true;
      else err = e.message;
    }
    try {
      apps = await get('/apps/projects');
    } catch {
      apps = null;
    }
  }
  onMount(load);

  function loaded(v) {
    cat = v;
    flash(t('apps.catalog.loaded', { serial: v.state.serial, apps: v.state.apps }));
  }

  async function refresh() {
    err = '';
    busy = true;
    try {
      loaded(await post('/apps/catalog/refresh'));
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }

  async function upload(file, allowRollback = false) {
    err = '';
    busy = true;
    try {
      const res = await fetch('/api/v1/apps/catalog/import' + (allowRollback ? '?allow_rollback=1' : ''), {
        method: 'POST', credentials: 'same-origin', body: file,
        headers: { 'Content-Type': 'application/octet-stream', 'X-CSRF-Token': csrfToken() },
      });
      const data = await res.json().catch(() => null);
      if (!res.ok) throw new ApiError(res.status, data?.error?.code || '', data?.error?.message || `HTTP ${res.status}`);
      older = null;
      loaded(data);
    } catch (e) {
      if (e.code === 'catalog_rollback' && !allowRollback) older = file;
      else err = e.message;
    }
    busy = false;
  }

  function pick(e) {
    const f = e.currentTarget.files?.[0];
    e.currentTarget.value = '';
    if (f) upload(f);
  }

  const needs = (m) => [m.min_ram_mb && t('apps.catalog.ram', { mb: m.min_ram_mb }), m.min_disk_mb && t('apps.catalog.disk', { mb: m.min_disk_mb })].filter(Boolean);
</script>

<div class="stack">
  <section class="card stack">
    <div class="row between">
      <h2>{t('apps.catalog.title')} <HelpButton section="apps.catalog" /></h2>
      {#if cat}
        <div class="row">
          {#if cat.source_url}<button disabled={busy} onclick={refresh}>{t('apps.catalog.refresh')}</button>{/if}
          <button disabled={busy} onclick={() => fileInput.click()}>{t('apps.catalog.import')}</button>
          <input bind:this={fileInput} type="file" accept=".tar.gz,.tgz,application/gzip" hidden onchange={pick} />
        </div>
      {/if}
    </div>
    {#if unavailable}<p class="banner warn">{t('apps.catalog.unavailable')}</p>{/if}
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    {#if older}
      <div class="banner warn stack"><span>{t('apps.catalog.olderAsk')}</span>
        <div class="row"><Confirm label={t('apps.catalog.olderConfirm')} disabled={busy} onconfirm={() => upload(older, true)} />
          <button onclick={() => (older = null)}>{t('common.cancel')}</button></div></div>
    {/if}
    {#if cat}
      {#if cat.state?.serial}
        <p class="small">{t('apps.catalog.state', { serial: cat.state.serial, apps: cat.state.apps, at: dateTime(cat.state.loaded_at),
          source: cat.state.source === 'file' ? t('apps.catalog.sourceFile') : cat.state.source })}</p>
      {:else}<p class="small muted">{t('apps.catalog.none')}</p>{/if}
      {#if cat.error}<p class="banner bad">{t('apps.catalog.error', { error: cat.error })}</p>{/if}
    {/if}
    {#if apps && !running}<p class="small muted">{t('apps.needRunning')}</p>{/if}
  </section>

  {#if cat?.state?.serial && !cat.error}
    <div class="catgrid">
      {#each cat.apps as e (e.manifest.id)}
        {@const m = e.manifest}
        {@const inst = installed[m.id] || []}
        <section class="card stack">
          <div class="row">
            <span class="caticon" aria-hidden="true">{textOf(m.name, lang()).slice(0, 1)}</span>
            <div><h3>{textOf(m.name, lang())}</h3>
              <span class="badge ok" title={t('apps.catalog.verifiedHint')}>{t('apps.catalog.verified')}</span></div>
          </div>
          <p class="small">{textOf(m.description, lang())}</p>
          <p class="small muted">{t('apps.catalog.version', { version: m.version, app: m.app_version })}
            {#if m.category?.length} · {m.category.join(', ')}{/if}
            {#if m.license} · {t('apps.catalog.license', { license: m.license })}{/if}</p>
          {#if needs(m).length}<p class="small muted">{t('apps.catalog.needs', { list: needs(m).join(', ') })}</p>{/if}
          {#if m.homepage}<a class="small" href={m.homepage} target="_blank" rel="noopener noreferrer">{t('apps.catalog.homepage')}</a>{/if}
          {#if inst.length}
            <p class="small">{t('apps.catalog.installed', { list: inst.map((p) => `${p.id} (${p.catalog.version})`).join(', ') })}
              {#each inst.filter((p) => hasUpdate(p, e)) as p}<span class="badge warn">{p.id}: {t('apps.catalog.updateAvail', { version: m.version })}</span>{/each}</p>
            <div class="row">
              {#each inst as p (p.id)}
                {#if hasUpdate(p, e)}
                  <button class="primary" onclick={() => { updating = true; detail = p.id; }}>{t('apps.catalog.updateTo', { id: p.id, version: m.version })}</button>
                {:else}
                  <button onclick={() => { updating = false; detail = p.id; }}>{t('apps.catalog.open', { id: p.id })}</button>
                {/if}
              {/each}
            </div>
          {/if}
          {#if m.update?.release_notes && textOf(m.update.release_notes, lang())}
            <details><summary class="small">{t('apps.catalog.notes')}</summary><p class="small">{textOf(m.update.release_notes, lang())}</p></details>
          {/if}
          {#if !inst.length}<div><button class="primary" disabled={!running} onclick={() => (install = e)}>{t('apps.catalog.install')}</button></div>{/if}
        </section>
      {:else}
        <p class="muted">{t('apps.catalog.empty')}</p>
      {/each}
    </div>
  {/if}
</div>

{#if install}
  <CatalogInstall entry={install} onclose={() => { install = null; load(); }} />
{/if}
{#if detail}
  <AppDetail id={detail} {running} autocheck={updating} onclose={() => { detail = ''; load(); }} onedit={() => {}} onmigrate={() => {}} />
{/if}

<style>
  .catgrid { display: grid; grid-template-columns: repeat(auto-fill, minmax(18rem, 1fr)); gap: 1rem; }
  .caticon { display: inline-grid; place-items: center; width: 2.5rem; height: 2.5rem; border-radius: 0.5rem;
    background: var(--accent-soft, var(--border)); font-weight: 700; font-size: 1.2rem; }
  h3 { margin: 0; }
</style>
