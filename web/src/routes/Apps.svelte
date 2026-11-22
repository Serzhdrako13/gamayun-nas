<script>
  // Раздел «Приложения» — подсистема, список, мастер, детали.
  import { onMount, onDestroy } from 'svelte';
  import { flash } from '../lib/state.svelte.js';
  import { get, post } from '../lib/api.js';
  import AppsCard from '../lib/AppsCard.svelte';
  import AppEditor from '../lib/AppEditor.svelte';
  import AppDetail from '../lib/AppDetail.svelte';
  import HelpButton from '../help/HelpButton.svelte';
  import { stateKind, portUrl, waitTask } from '../lib/apps.js';
  import { t } from '../lib/i18n/index.js';

  let data = $state(null);
  let err = $state('');
  let editor = $state(null); // null | {} — новое | {id} — изменение
  let detail = $state('');
  let busy = $state('');
  let timer;
  const host = typeof location !== 'undefined' ? location.hostname : '';
  const running = $derived(data?.apps?.state === 'running');

  async function load() {
    try {
      data = await get('/apps/projects');
      err = '';
    } catch (e) {
      err = e.message;
    }
    clearTimeout(timer);
    const fast = data && (data.apps?.state === 'starting' || data.projects.some((p) => p.state === 'starting'));
    timer = setTimeout(load, fast ? 3000 : 15000);
  }
  onMount(load);
  onDestroy(() => clearTimeout(timer));

  async function action(id, act) {
    busy = id;
    err = '';
    try {
      const tk = await waitTask(get, await post(`/apps/projects/${encodeURIComponent(id)}/${act}`));
      if (tk.state === 'succeeded') flash(t('apps.done.' + act, { id }));
      else if (tk.state === 'failed') err = t('apps.taskFailed', { error: tk.error?.message || '' });
      else flash(t('apps.taskRunning'));
    } catch (e) {
      err = e.message;
    }
    busy = '';
    load();
  }
</script>

<div class="stack">
  <AppsCard v={data?.apps} onchange={load} />
  {#if err}<p class="error" role="alert">{err}</p>{/if}
  {#if data?.apps?.enabled}
    <section class="card stack">
      <div class="row between">
        <h2>{t('apps.list')} <HelpButton section="apps.states" /></h2>
        <button disabled={!running} onclick={() => (editor = {})}>{t('apps.add')}</button>
      </div>
      {#if !running}<p class="small muted">{t('apps.needRunning')}</p>{/if}
      {#if data.foreign}<p class="small muted">{t('apps.foreign', { count: data.foreign })}</p>{/if}
      <div class="tablewrap">
        <table>
          <thead><tr><th>{t('apps.col.name')}</th><th>{t('apps.col.state')}</th><th>{t('apps.col.ports')}</th><th>{t('apps.col.actions')}</th></tr></thead>
          <tbody>
            {#each data.projects as p}
              <tr>
                <td>
                  <button class="link" onclick={() => (detail = p.id)}>{p.name}</button>
                  <span class="small muted">{p.id}</span>
                  {#if p.catalog}<span class="badge ok" title={t('apps.catalog.verifiedHint')}>{t('apps.catalog.badge')} {p.catalog.version}</span>
                  {:else}<span class="badge" title={t('apps.userHint')}>{t('apps.user')}</span>{/if}
                  {#if p.legacy_root_share}<span class="badge warn" title={t('apps.access.legacy')}>{t('apps.access.rootBadge')}</span>{/if}
                </td>
                <td><span class="badge {stateKind(p.state)}" title={p.error || ''}>{t('apps.pstate.' + p.state)}</span></td>
                <td>
                  {#if p.web && data.domain}
                    <a class="small" href={`https://${p.web.name}-${data.domain}/`} target="_blank" rel="noopener">{p.web.name}-{data.domain}</a><br />
                  {/if}
                  {#each p.ports || [] as port}
                    {@const url = portUrl(port, host)}
                    <span class="small">{#if url}<a href={url} target="_blank" rel="noopener">{port.host}/{port.proto}</a>{:else}{port.host}/{port.proto}{/if}
                      <span class="muted">({port.network ? t('apps.inNetwork') : t('apps.localOnly')})</span></span><br />
                  {/each}
                </td>
                <td class="row">
                  {#if p.state !== 'removed'}
                    {#if p.desired === 'stopped' || p.crashing}
                      <button disabled={!running || busy === p.id} onclick={() => action(p.id, 'start')}>{t('apps.start')}</button>
                    {:else}
                      <button disabled={!running || busy === p.id} onclick={() => action(p.id, 'stop')}>{t('apps.stop')}</button>
                      <button disabled={!running || busy === p.id} onclick={() => action(p.id, 'restart')}>{t('apps.restart')}</button>
                    {/if}
                  {/if}
                  <button onclick={() => (detail = p.id)}>{t('apps.open')}</button>
                </td>
              </tr>
            {:else}
              <tr><td colspan="4" class="muted">{t('apps.empty')}</td></tr>
            {/each}
          </tbody>
        </table>
      </div>
    </section>
  {/if}
</div>

{#if editor}
  <AppEditor edit={editor.id ? editor : null} migrate={!!editor.migrate} onclose={() => { editor = null; load(); }} />
{/if}
{#if detail}
  <AppDetail id={detail} {running} onclose={() => { detail = ''; load(); }} onedit={(d) => { detail = ''; editor = d; }}
    onmigrate={(d) => { detail = ''; editor = { ...d, migrate: true }; }} />
{/if}
