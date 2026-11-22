<script>
  import HelpButton from '../help/HelpButton.svelte';
  // Снапшоты общей папки — расписание, список, ручное создание и
  // удаление, просмотр снапшота и восстановление копией рядом с исходным местом.
  import { onMount } from 'svelte';
  import { flash } from './state.svelte.js';
  import { get, post, put, del } from './api.js';
  import Confirm from './Confirm.svelte';
  import Switch from './Switch.svelte';
  import Icon from './Icon.svelte';
  import { bytes } from './format.js';
  import { snapTime, crumbs, joinPath, LEVELS, policyError, policySummary, restoreText } from './snapshots.js';
  import { t } from './i18n/index.js';

  let { share } = $props();
  const enc = encodeURIComponent;
  const base = $derived(`/nas/shares/${enc(share)}/snapshots`);

  let info = $state(null);
  let err = $state('');
  let pol = $state(null); // редактируемая копия расписания
  let busy = $state(false);
  // Просмотр снапшота: {name, path, entries, partial} | null
  let view = $state(null);
  let restoring = $state('');

  async function load() {
    try {
      info = await get(base);
      pol = { ...info.policy };
      err = '';
    } catch (e) {
      err = e.message;
    }
  }
  onMount(load);

  async function act(fn, ok) {
    err = '';
    busy = true;
    try {
      await fn();
      if (ok) flash(ok);
      await load();
    } catch (e) {
      err = e.message;
    } finally {
      busy = false;
    }
  }

  const perr = $derived(pol ? policyError(pol) : '');
  const savePolicy = () => act(() => put(`/nas/shares/${enc(share)}/snapshot-policy`, pol), t('snapshots.policySaved'));
  const createNow = () => act(() => post(base), t('snapshots.created'));
  const remove = (name) =>
    act(async () => {
      await del(`${base}/${enc(name)}`);
      if (view && view.name === name) view = null;
    }, t('snapshots.deleted'));

  async function open(name, path = '') {
    err = '';
    try {
      const r = await get(`${base}/${enc(name)}/files?path=${enc(path)}`);
      view = { name, path, entries: r.entries || [], partial: !!r.partial };
    } catch (e) {
      err = e.message;
    }
  }

  async function restore(path) {
    err = '';
    restoring = path;
    try {
      let tk = await post(`${base}/${enc(view.name)}/restore`, { path });
      const until = Date.now() + 35 * 60 * 1000;
      while ((tk.state === 'queued' || tk.state === 'running') && Date.now() < until) {
        await new Promise((r) => setTimeout(r, 1000));
        tk = await get(`/tasks/${enc(tk.id)}`);
      }
      if (tk.state === 'succeeded') flash(restoreText(tk.result));
      else if (tk.state === 'queued' || tk.state === 'running') err = t('snapshots.restoreRunning');
      else err = t('snapshots.restoreFailed', { error: tk.error?.message || tk.state });
    } catch (e) {
      err = e.message;
    } finally {
      restoring = '';
    }
  }
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
{#if !info}
  {#if !err}<p class="muted">{t('common.loading')}</p>{/if}
{:else}
  <div class="stack">
    {#if !info.subvolume}
      <p class="banner warn">{t('snapshots.notSubvolume')}</p>
    {/if}
    {#if info.read_only}<p class="banner warn">{t('snapshots.readOnly')}</p>{/if}
    <p class="small muted">
      {t('snapshots.poolFree', { pool: info.pool, pct: Math.round(info.free_pct) })}
      {#if info.free_pct < 10}<span class="badge warn">{t('snapshots.lowSpace')}</span>{/if}
    </p>

    {#if info.subvolume && pol}
      <section class="stack">
        <h3>{t('snapshots.schedule')} <HelpButton section="shares" /></h3>
        <Switch label={t('snapshots.scheduled')} hint={policySummary(info.policy)} checked={pol.enabled} onchange={(v) => (pol.enabled = v)} />
        <div class="row">
          {#each LEVELS as [k, label, max]}
            <label class="field"><span>{label}</span><input type="number" min="0" {max} step="1" bind:value={pol[k]} aria-invalid={!!perr} /></label>
          {/each}
        </div>
        {#if perr}<p class="error small" role="alert">{perr}</p>{/if}
        <div class="row">
          <button class="primary" disabled={busy || !!perr} onclick={savePolicy}>{t('snapshots.savePolicy')}</button>
          <button disabled={busy || info.read_only} onclick={createNow}>{t('snapshots.createNow')}</button>
        </div>
      </section>
    {/if}

    <section class="stack">
      <h3>{t('snapshots.title')} <HelpButton section="shares" /> <span class="tag">{info.snapshots.length}</span></h3>
      {#if info.snapshots.length}
        <div class="tablewrap">
          <table>
            <thead><tr><th>{t('snapshots.colTime')}</th><th>{t('snapshots.colKind')}</th><th></th></tr></thead>
            <tbody>
              {#each info.snapshots as s (s.name)}
                <tr>
                  <td><span title={s.name}>{snapTime(s.time)}</span></td>
                  <td>{s.auto ? t('snapshots.auto') : t('snapshots.manual')}</td>
                  <td class="row">
                    <button onclick={() => open(s.name)} aria-pressed={view?.name === s.name}>{t('common.open')}</button>
                    <Confirm label={t('common.delete')} aria={t('snapshots.deleteAria', { time: snapTime(s.time) })} disabled={busy} onconfirm={() => remove(s.name)} />
                  </td>
                </tr>
              {/each}
            </tbody>
          </table>
        </div>
      {:else}
        <p class="muted">{t('snapshots.none')}</p>
      {/if}
    </section>

    {#if view}
      <section class="stack">
        <h3>{t('snapshots.viewTitle', { time: snapTime(info.snapshots.find((s) => s.name === view.name)?.time) })} <span class="tag">{t('common.readOnly')}</span></h3>
        <nav class="row small" aria-label={t('snapshots.pathAria')}>
          {#each crumbs(view.path) as c, i}
            {#if i > 0}<span class="muted">/</span>{/if}
            <button class="link" onclick={() => open(view.name, c.path)}>{c.name}</button>
          {/each}
        </nav>
        <ul class="tree">
          {#each view.entries as e (e.name)}
            <li>
              <div>
                {#if e.type === 'dir'}
                  <button class="link rootbtn" onclick={() => open(view.name, joinPath(view.path, e.name))}><Icon name="folder" /><b>{e.name}</b></button>
                {:else}
                  <span class="sp">{e.name} <span class="muted small">{e.type === 'file' ? bytes(e.size) : e.type === 'link' ? t('snapshots.link') : t('snapshots.special')}</span></span>
                {/if}
                {#if e.type !== 'other'}
                  <button disabled={!!restoring} onclick={() => restore(joinPath(view.path, e.name))}>{restoring === joinPath(view.path, e.name) ? t('snapshots.restoring') : t('snapshots.restore')}</button>
                {/if}
              </div>
            </li>
          {:else}
            <li class="muted">{t('snapshots.empty')}</li>
          {/each}
        </ul>
        {#if view.partial}<p class="small muted">{t('snapshots.partial')}</p>{/if}
      </section>
    {/if}

  </div>
{/if}
