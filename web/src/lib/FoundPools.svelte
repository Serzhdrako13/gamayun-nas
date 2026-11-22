<script>
  // Пулы GamayunNas на дисках после переустановки системы — подхват.
  // Подтверждение — имя пула; неполный пул — отдельное явное согласие.
  import { onMount } from 'svelte';
  import { get, post } from './api.js';
  import { flash, refresh } from './state.svelte.js';
  import { waitTask } from './apps.js';
  import { bytes } from './format.js';
  import HelpButton from '../help/HelpButton.svelte';
  import { t } from './i18n/index.js';

  let { onimported = () => {} } = $props();
  let pools = $state([]);
  let confirm = $state({});
  let degraded = $state({});
  let busy = $state('');
  let err = $state({});

  async function load() {
    try {
      pools = (await get('/storage/found-pools')).pools || [];
    } catch {
      pools = [];
    }
  }
  onMount(load);

  async function doImport(p) {
    busy = p.name;
    err = { ...err, [p.name]: '' };
    try {
      const r = await post(`/storage/found-pools/${encodeURIComponent(p.name)}/import`, { confirm_name: confirm[p.name] || '', degraded: !!degraded[p.name] });
      const tk = await waitTask(get, r.task);
      if (tk.state !== 'succeeded') throw new Error(tk.error || t('foundPools.failed'));
      flash(t('foundPools.done', { name: p.name }));
      await refresh();
      await load();
      onimported(p.name, tk.result);
    } catch (x) {
      err = { ...err, [p.name]: x.message };
    } finally {
      busy = '';
    }
  }
</script>

{#if pools.length}
  <section class="card stack">
    <h2>{t('foundPools.title')} <HelpButton section="storage.pools.found" /></h2>
    {#each pools as p (p.name)}
      <div class="stack">
        <div class="row between">
          <b>{p.name}</b>
          <span class="muted small">{t('foundPools.disks', { n: p.disks })}{p.integrity ? t('foundPools.integrity') : ''}</span>
        </div>
        <div class="muted small">{p.arrays.flatMap((a) => a.members).filter((m, i, all) => all.findIndex((x) => x.disk_id === m.disk_id) === i).map((m) => `${m.disk_name} (${bytes(m.size_bytes)})`).join(', ')}</div>
        {#if p.known}
          <div class="banner warn">{t('foundPools.known')}</div>
        {:else if p.busy}
          <div class="banner warn">{t('foundPools.busy')}</div>
        {:else}
          <label class="field"><span>{t('foundPools.confirm', { name: p.name })}</span>
            <input bind:value={confirm[p.name]} autocomplete="off" spellcheck="false" />
          </label>
          <label class="row"><input type="checkbox" bind:checked={degraded[p.name]} /> <span class="small">{t('foundPools.degraded')}</span></label>
          <div class="row"><button class="primary" disabled={busy !== '' || confirm[p.name] !== p.name} onclick={() => doImport(p)}>{busy === p.name ? t('foundPools.importing') : t('foundPools.import')}</button></div>
        {/if}
        {#if err[p.name]}<div class="banner error">{err[p.name]}</div>{/if}
      </div>
    {/each}
  </section>
{/if}
