<script>
  // Публикация приложения через прокси (Caddy) на поддомене со входом по учётке NAS.
  import { onMount } from 'svelte';
  import { get, put, del } from './api.js';
  import { flash } from './state.svelte.js';
  import HelpButton from '../help/HelpButton.svelte';
  import { t } from './i18n/index.js';

  let { d, onchange } = $props();
  let px = $state(null);
  let pxErr = $state('');
  let name = $state('');
  let port = $state(0);
  let busy = $state(false);
  let err = $state('');

  const local = $derived((d.ports || []).filter((p) => p.proto === 'tcp' && !p.network));
  const domain = $derived(px?.domain || '');

  onMount(async () => {
    name = d.web?.name || d.id.toLowerCase().replace(/[^a-z0-9-]/g, '-').replace(/^-+|-+$/g, '').slice(0, 32);
    port = d.web?.port || local[0]?.host || 0;
    try {
      px = await get('/apps/proxy');
    } catch (e) {
      pxErr = e.message;
    }
  });

  async function publish() {
    busy = true;
    err = '';
    try {
      await put(`/apps/projects/${encodeURIComponent(d.id)}/web`, { name, port: Number(port) });
      flash(t('apps.web.published', { url: `https://${name}-${domain}/` }));
      onchange();
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }
  async function unpublish() {
    busy = true;
    err = '';
    try {
      await del(`/apps/projects/${encodeURIComponent(d.id)}/web`);
      flash(t('apps.web.unpublished'));
      onchange();
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }
</script>

<h3>{t('apps.web.title')} <HelpButton section="apps.web" /></h3>
{#if err}<p class="error" role="alert">{err}</p>{/if}
{#if pxErr}<p class="small muted">{pxErr}</p>
{:else if px && !px.available}<p class="banner warn">{t('apps.web.noProxy')}</p>
{:else if px?.error}<p class="banner bad">{t('apps.web.proxyError', { error: px.error })}</p>{/if}
{#if d.web}
  {@const url = `https://${d.web.name}-${domain}/`}
  <p class="small">{t('apps.web.at')} <a href={url} target="_blank" rel="noopener">{url}</a> → 127.0.0.1:{d.web.port}</p>
  <div class="row"><button disabled={busy} onclick={unpublish}>{t('apps.web.unpublish')}</button></div>
{:else if local.length}
  <div class="row">
    <label class="field"><span>{t('apps.web.name')}</span><input bind:value={name} maxlength="32" /></label>
    <span class="small muted">-{domain}</span>
    <label class="field"><span>{t('apps.web.port')}</span>
      <select bind:value={port}>{#each local as p}<option value={p.host}>{p.host} ({p.service})</option>{/each}</select></label>
    <button class="primary" disabled={busy || !name || !port || (px && !px.available)} onclick={publish}>{t('apps.web.publish')}</button>
  </div>
{:else}
  <p class="small muted">{t('apps.web.noPorts')}</p>
{/if}
{#if px?.available}
  <p class="small muted">{t('apps.web.caHint')} <a href="/api/v1/apps/proxy/ca.crt" download>{t('apps.web.ca')}</a></p>
{/if}
