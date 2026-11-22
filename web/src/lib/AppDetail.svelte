<script>
  // Детали приложения — контейнеры, риски, compose, журнал, удаление.
  import { onMount } from 'svelte';
  import { flash } from './state.svelte.js';
  import { get, del } from './api.js';
  import Drawer from './Drawer.svelte';
  import Confirm from './Confirm.svelte';
  import HelpButton from '../help/HelpButton.svelte';
  import AppWeb from './AppWeb.svelte';
  import AppCatalog from './AppCatalog.svelte';
  import { stateKind, waitTask } from './apps.js';
  import { dateTime } from './format.js';
  import { t } from './i18n/index.js';

  let { id, running, onclose, onedit, onmigrate, autocheck = false } = $props();
  let d = $state(null);
  let err = $state('');
  let keep = $state(true);
  let busy = $state(false);
  let svc = $state('');
  let tail = $state(200);
  let logs = $state(null);
  let logErr = $state('');

  async function load() {
    try {
      d = await get(`/apps/projects/${encodeURIComponent(id)}`);
      if (!svc) svc = d.services?.[0] || '';
      err = '';
    } catch (e) {
      err = e.message;
    }
  }
  onMount(load);

  const editArgs = () => ({ id: d.id, name: d.name, compose: d.compose, env_keys: d.env_keys, network: d.network || [],
    uid: d.uid || 0, run_as: d.run_as || {}, grants: d.grants || [] });

  async function loadLogs() {
    logErr = '';
    try {
      logs = await get(`/apps/projects/${encodeURIComponent(id)}/logs?service=${encodeURIComponent(svc)}&tail=${tail}`);
    } catch (e) {
      logErr = e.message;
      logs = null;
    }
  }

  function download() {
    const a = document.createElement('a');
    a.href = URL.createObjectURL(new Blob([logs?.text || ''], { type: 'text/plain' }));
    a.download = `${id}-${svc}.log`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }

  async function remove() {
    busy = true;
    err = '';
    try {
      const tk = await waitTask(get, await del(`/apps/projects/${encodeURIComponent(id)}?keep_data=${keep ? 1 : 0}`));
      if (tk.state === 'succeeded') {
        flash(t('apps.done.remove', { id }));
        onclose();
      } else if (tk.state === 'failed') err = t('apps.taskFailed', { error: tk.error?.message || '' });
      else flash(t('apps.taskRunning'));
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }
</script>

<Drawer title={d?.name || id} {onclose}>
  <div class="stack">
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    {#if d}
      <div class="row">
        <span class="badge {stateKind(d.state)}">{t('apps.pstate.' + d.state)}</span>
        {#if d.catalog}<span class="badge ok" title={t('apps.catalog.verifiedHint')}>{t('apps.catalog.badge')}</span>
        {:else}<span class="badge" title={t('apps.userHint')}>{t('apps.user')}</span>{/if}
        <HelpButton section="apps.states" />
      </div>
      {#if d.error}<p class="banner bad">{t('apps.detail.error', { error: d.error })}</p>{/if}
      {#if d.state !== 'removed' && !d.uid}
        <div class="banner warn stack">
          <span>{d.legacy_root_share ? t('apps.access.legacy') : t('apps.access.noUser')} <HelpButton section="apps.access" /></span>
          <div><button disabled={!running} onclick={() => onmigrate(editArgs())}>{t('apps.access.migrate')}</button></div>
        </div>
      {/if}
      {#if d.uid}
        <h3>{t('apps.access.title')} <HelpButton section="apps.access" /></h3>
        {#each Object.entries(d.run_as || {}).filter(([, m]) => m === 'root') as [s]}<p class="banner warn">{s}: {t('apps.access.mode.root')}</p>{/each}
        {#if d.grants?.length}
          <ul class="small">{#each d.grants as g}<li><b>{g.share}</b> — {g.mode === 'rw' ? t('apps.access.rw') : t('apps.access.ro')}
            <span class="muted">({t('apps.detail.riskBy', { by: g.by, at: dateTime(g.at) })})</span></li>{/each}</ul>
        {:else}<p class="small muted">{t('apps.access.none')}</p>{/if}
        {#if d.access_drift?.length}<p class="banner warn">{t('apps.access.drift', { list: d.access_drift.join(', ') })}</p>{/if}
      {/if}
      <p class="small muted">{t('apps.detail.created', { at: dateTime(d.created) })}</p>
      {#if d.state !== 'removed'}<AppWeb {d} onchange={load} />{/if}
      {#if d.catalog && d.state !== 'removed'}{#key d.catalog.version}<AppCatalog {d} {running} {autocheck} onchange={load} />{/key}{/if}

      <h3>{t('apps.detail.containers')}</h3>
      {#if d.containers.length}
        <div class="tablewrap"><table>
          <tbody>{#each d.containers as c}
            <tr><td>{c.name}</td><td class="small">{c.image}</td><td class="small">{c.status}</td></tr>
          {/each}</tbody>
        </table></div>
      {:else}<p class="small muted">{t('apps.detail.noContainers')}</p>{/if}

      {#if d.risks?.length}
        <h3>{t('apps.detail.risks')} <HelpButton section="apps.risks" /></h3>
        <ul class="small">{#each d.risks as r}<li>{r.detail} — <span class="muted">{t('apps.detail.riskBy', { by: r.by, at: dateTime(r.at) })}</span></li>{/each}</ul>
      {/if}

      {#if d.state !== 'removed'}
        <details><summary>{t('apps.detail.compose')}</summary><pre class="mono small">{d.compose}</pre>
          {#if d.env_keys.length}<p class="small muted">{t('apps.detail.envKeys', { keys: d.env_keys.join(', ') })}</p>{/if}</details>
        {#if !d.catalog}<div><button disabled={!running} onclick={() => onedit(editArgs())}>{t('apps.edit')}</button></div>{/if}

        <h3>{t('apps.detail.logs')}</h3>
        <div class="row">
          <label class="field"><span>{t('apps.detail.service')}</span><select bind:value={svc}>{#each d.services as s}<option>{s}</option>{/each}</select></label>
          <label class="field"><span>{t('apps.detail.lines')}</span><select bind:value={tail}>{#each [100, 200, 1000, 5000] as n}<option value={n}>{n}</option>{/each}</select></label>
          <button disabled={!running || !svc} onclick={loadLogs}>{t('apps.detail.refresh')}</button>
          {#if logs}<button onclick={download}>{t('apps.detail.download')}</button>{/if}
        </div>
        {#if logErr}<p class="error">{logErr}</p>{/if}
        {#if logs}
          {#if logs.truncated}<p class="small muted">{t('apps.detail.truncated')}</p>{/if}
          <pre class="mono small logbox">{logs.text}</pre>
        {/if}
      {/if}

      <h3>{t('apps.remove')}</h3>
      {#if d.state !== 'removed'}<label class="row"><input type="checkbox" bind:checked={keep} /> {t('apps.keepData')}</label>{/if}
      <p class="small muted">{t('apps.removeHint')}</p>
      <div><Confirm label={t('apps.remove')} disabled={busy || !running} onconfirm={() => { if (d.state === 'removed') keep = false; remove(); }} /></div>
    {:else if !err}<p class="muted">{t('common.loading')}</p>{/if}
  </div>
</Drawer>

<style>
  .logbox { max-height: 24rem; overflow: auto; white-space: pre-wrap; word-break: break-all; }
</style>
