<script>
  import { onMount } from 'svelte';
  import { app } from '../lib/state.svelte.js';
  import { bytes, POOL_TYPES, protectionBanner } from '../lib/format.js';
  import PoolBadge from '../lib/PoolBadge.svelte';
  import PoolDetail from '../lib/PoolDetail.svelte';
  import PoolWizard from '../lib/PoolWizard.svelte';
  import FoundPools from '../lib/FoundPools.svelte';
  import { t } from '../lib/i18n/index.js';

  let open = $state('');
  let wizard = $state(false);

  // Состояние drawer хранится в адресе: #/pools?new, #/pools?open=имя.
  function sync() {
    const q = new URLSearchParams(location.hash.split('?')[1] || '');
    wizard = q.has('new');
    open = q.get('open') || '';
  }
  onMount(() => {
    sync();
    window.addEventListener('hashchange', sync);
    return () => window.removeEventListener('hashchange', sync);
  });
  const go = (q) => (location.hash = '#/pools' + (q ? '?' + q : ''));
</script>

<div class="row between" style:margin-bottom="16px">
  <span class="muted">{t('pools.intro')}</span>
  <button class="primary" onclick={() => go('new')}>{t('common.createPool')}</button>
</div>

<FoundPools />

<div class="grid">
  {#each app.pools as p}
    {@const b = protectionBanner(p)}
    <section class="card stack">
      <div class="row between"><h2>{p.name}</h2><PoolBadge pool={p} /></div>
      <div class="muted small">{POOL_TYPES[p.type] || p.type}{p.integrity ? t('pools.integrity') : ''}{t('pools.disks', { n: p.disks?.length ?? 0 })}{bytes((p.disks || []).reduce((a, d) => a + d.size_bytes, 0))}</div>
      {#if b}<div class="banner {b.kind}">{b.text}</div>{/if}
      <button onclick={() => go('open=' + encodeURIComponent(p.name))}>{t('common.open')}</button>
    </section>
  {:else}
    <p class="muted">{t('pools.empty')}</p>
  {/each}
</div>

{#if wizard}<PoolWizard onclose={() => go('')} />{/if}
{#if open}<PoolDetail name={open} onclose={() => go('')} />{/if}
