<script>
  import { onMount } from 'svelte';
  import { app } from '../lib/state.svelte.js';
  import { get } from '../lib/api.js';
  import { bytes, POOL_TYPES, protectionBanner, health, freeDisks, dateTime } from '../lib/format.js';
  import PoolBadge from '../lib/PoolBadge.svelte';
  import PoolDetail from '../lib/PoolDetail.svelte';
  import { smartCounts } from '../lib/smart.js';
  import { t } from '../lib/i18n/index.js';
  import HelpButton from '../help/HelpButton.svelte';
  import { poolTopic } from '../help/topic.js';

  let snap = $state(null);
  let snapErr = $state('');
  let open = $state('');

  onMount(async () => {
    try {
      snap = await get('/disks');
    } catch (e) {
      snapErr = e.message;
    }
  });


  const banners = $derived(app.pools.map((p) => ({ p, b: protectionBanner(p) })).filter((x) => x.b));
  const attention = $derived(app.pools.filter((p) => ['warning', 'degraded', 'failed', 'missing', 'unknown'].includes(p.status?.health)));
  const sc = $derived(smartCounts(snap?.disks));
  const running = $derived(app.tasks.filter((t) => t.state === 'running' || t.state === 'queued'));
  const STATE = $derived(Object.fromEntries(['queued', 'running', 'succeeded', 'failed', 'interrupted'].map((k) => [k, t('home.taskState.' + k)])));
</script>

{#each banners as { p, b }}
  <div class="banner {b.kind}" role="alert">
    <div class="row between">
      <span><b>{t('home.poolPrefix', { name: p.name })}</b> {b.text} <HelpButton section="storage.pools.readonly" /></span>
      <button onclick={() => (open = p.name)}>{b.override || b.off ? t('home.details') : t('home.allowWrite')}</button>
    </div>
  </div>
{/each}
{#each attention as p}
  <div class="banner warn" role="alert"><b>{t('home.poolPrefix', { name: p.name })}</b> {health(p)[0]}. <button class="link" onclick={() => (open = p.name)}>{t('common.open')}</button> <HelpButton section={poolTopic(p)} /></div>
{/each}

<div class="grid home">
  <section class="card c6">
    <h3>{t('common.state')}</h3>
    {#if app.poolsError}
      <div class="rowi"><span class="dot b"></span><div class="sp"><b>{t('home.noConnection')}</b><div class="muted">{app.poolsError}</div></div></div>
    {:else if !attention.length && !banners.length}
      <div class="rowi"><span class="dot"></span><div class="sp"><b>{t('home.allGood')}</b><div class="muted">{app.pools.length ? t('home.poolsNoIssues', { n: app.pools.length }) : t('home.noPoolsYet')}</div></div></div>
    {:else}
      <div class="rowi"><span class="dot w"></span><div class="sp"><b>{t('home.issues', { n: attention.length + banners.length })}</b><div class="muted">{t('home.issuesHint')}</div></div></div>
    {/if}
  </section>

  <section class="card c6">
    <div class="row between"><h3>{t('home.pools')}</h3><a href="#/pools?new">{t('common.createPool')}</a></div>
    {#if !app.pools.length}
      <p class="muted">{t('home.noPools')}</p>
    {/if}
    {#each app.pools as p}
      <button class="link rowi" onclick={() => (open = p.name)}>
        <span class="sp"><b>{p.name}</b> <span class="muted small">{POOL_TYPES[p.type] || p.type}</span></span>
        <PoolBadge pool={p} />
      </button>
    {/each}
  </section>

  <section class="card c6">
    <div class="row between"><h3>{t('home.disks')}</h3><a href="#/pools">{t('home.allDisks')}</a></div>
    {#if snapErr}<p class="error">{snapErr}</p>
    {:else if !snap}<p class="muted">{t('common.loading')}</p>
    {:else}
      <div class="big">{snap.disks.length}</div>
      <p class="muted">{t('home.freeDisks')}<b>{freeDisks(snap).length}</b></p>
      <p class="small">SMART:
        {#if sc.critical}<span class="badge bad">{t('home.smart.critical', { n: sc.critical })}</span>{/if}
        {#if sc.warning}<span class="badge warn">{t('home.smart.warning', { n: sc.warning })}</span>{/if}
        {#if sc.ok}<span class="badge ok">{t('home.smart.ok', { n: sc.ok })}</span>{/if}
        {#if sc.unknown}<span class="badge">{t('home.smart.unknown', { n: sc.unknown })}</span>{/if}
        {#if sc.maxTemp != null}<span class="muted">{t('home.smart.maxTemp', { n: sc.maxTemp })}</span>{/if}
      </p>
      {#each sc.problems as p}<p class="small"><b>{p.name}</b>: {p.reasons.join('; ')}</p>{/each}
      {#if !snap.complete}<p class="small muted">{t('home.incomplete')}</p>{/if}
    {/if}
  </section>

  <section class="card c6">
    <h3>{t('home.tasks')}</h3>
    {#if running.length}
      {#each running as t}
        <div class="stack">
          <div>{t.kind} — {STATE[t.state]} {t.progress}%</div>
          <div class="bar"><i style:width={t.progress + '%'}></i></div>
        </div>
      {/each}
    {:else}
      <p class="muted">{t('home.noTasks')}</p>
    {/if}
    <ul class="small">
      {#each app.tasks.slice(0, 5) as t}
        <li>{dateTime(t.created_at)} — {t.kind}: {STATE[t.state] || t.state}{t.error ? ' (' + t.error.message + ')' : ''}</li>
      {/each}
    </ul>
  </section>

</div>

{#if open}<PoolDetail name={open} onclose={() => (open = '')} />{/if}
