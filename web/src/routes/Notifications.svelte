<script>
  import HelpButton from '../help/HelpButton.svelte';
  // Центр уведомлений — лента событий оборудования и предупреждений.
  import { onMount } from 'svelte';
  import { app, flash } from '../lib/state.svelte.js';
  import { get, post } from '../lib/api.js';
  import { notifyText, SEVERITY } from '../shell/events.js';
  import QuietCard from '../lib/QuietCard.svelte';
  import { t, locale } from '../lib/i18n/index.js';

  let items = $state([]);
  let more = $state(false);
  let unreadOnly = $state(false);
  let err = $state('');
  const PAGE = 50;

  async function load(append = false) {
    err = '';
    try {
      const before = append && items.length ? items[items.length - 1].id : 0;
      const r = await get(`/notifications?limit=${PAGE}&before=${before}${unreadOnly ? '&unread=1' : ''}`);
      items = append ? [...items, ...r.items] : r.items;
      more = r.items.length === PAGE;
      app.unread = r.unread;
    } catch (e) {
      err = e.message;
    }
  }
  onMount(() => load());

  async function readAll() {
    try {
      const top = items[0]?.id || 0;
      app.unread = (await post('/notifications/read', { up_to: top })).unread;
      await load();
      flash(t('notifications.readDone'));
    } catch (e) {
      err = e.message;
    }
  }
  const time = (x) => new Date(x).toLocaleString(locale());
  function details(n) {
    if (!n.data || typeof n.data !== 'object') return '';
    return Object.entries(n.data)
      .filter(([, v]) => v !== null && v !== '' && typeof v !== 'object')
      .map(([k, v]) => k + ': ' + v)
      .join(', ');
  }
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
<QuietCard />
<section class="card stack">
  <div class="row between">
    <label class="row"><input type="checkbox" bind:checked={unreadOnly} onchange={() => load()} /> {t('notifications.unreadOnly')}</label>
    <span class="row">
      <button onclick={() => load()}>{t('common.refresh')}</button>
      <button class="primary" disabled={!app.unread} onclick={readAll}>{t('notifications.readAll')}</button>
    </span>
  </div>
  {#if !items.length}
    <p class="muted">{t('notifications.empty')} <HelpButton section="notify" /></p>
  {:else}
    <ul class="nlist">
      {#each items as n (n.id)}
        <li class:unread={!n.read_at}>
          <span class="badge {SEVERITY[n.severity]?.[1] || ''}">{SEVERITY[n.severity]?.[0] || n.severity}</span>
          <span class="nt"><b>{notifyText(n)}</b>{#if details(n)}<small class="muted">{details(n)}</small>{/if}</span>
          <small class="muted">{time(n.time)}</small>
        </li>
      {/each}
    </ul>
    {#if more}<button onclick={() => load(true)}>{t('common.showMore')}</button>{/if}
  {/if}
</section>

<style>
  .nlist { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 6px; }
  .nlist li { display: grid; grid-template-columns: auto 1fr auto; gap: 10px; align-items: center; padding: 8px 10px; border-radius: 8px; border: 1px solid var(--line); }
  .nlist li.unread { border-left: 3px solid var(--warn); }
  .nt { display: flex; flex-direction: column; min-width: 0; overflow-wrap: anywhere; }
  @media (max-width: 600px) { .nlist li { grid-template-columns: 1fr; } }
</style>
