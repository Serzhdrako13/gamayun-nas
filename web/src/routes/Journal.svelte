<script>
  // Журнал событий: лента /events/recent, новые первыми.
  // #/journal?event=<id> — прокрутить к событию и подсветить его.
  import { onMount, tick } from 'svelte';
  import { get } from '../lib/api.js';
  import { eventText, eventKind } from '../shell/events.js';
  import { eventParam } from '../shell/power.js';
  import { t, locale } from '../lib/i18n/index.js';

  const PAGE = 50;
  let items = $state([]);
  let more = $state(false);
  let err = $state('');
  let mark = $state(0);
  let notFound = $state(false);
  const rows = {};

  async function page(before) {
    const r = await get(`/events/recent?limit=${PAGE}${before ? '&before=' + before : ''}`);
    return r.events || [];
  }
  async function load(append = false) {
    err = '';
    try {
      const evs = await page(append && items.length ? items[items.length - 1].id : 0);
      items = append ? [...items, ...evs] : evs;
      more = evs.length === PAGE;
    } catch (e) {
      err = e.message;
    }
  }
  // Подсветка: догружаем страницы, пока событие не найдено (до 10 страниц).
  async function focus() {
    mark = eventParam(location.hash);
    notFound = false;
    if (!mark) return;
    if (!items.length) await load();
    for (let i = 0; i < 10 && more && !items.some((e) => e.id === mark) && items[items.length - 1].id > mark; i++) await load(true);
    if (!items.some((e) => e.id === mark)) { notFound = true; return; }
    await tick();
    rows[mark]?.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }
  onMount(() => {
    load().then(focus);
    const on = () => focus();
    addEventListener('hashchange', on);
    return () => removeEventListener('hashchange', on);
  });
  const time = (v) => new Date(v).toLocaleString(locale());
  const SEV = { info: 'ok', warning: 'warn', critical: 'bad' };
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
<section class="card stack">
  <div class="row between">
    <span class="muted small">{t('journal.intro')}</span>
    <button onclick={() => load().then(focus)}>{t('common.refresh')}</button>
  </div>
  {#if notFound}<p class="muted" role="status">{t('journal.notFound', { id: mark })}</p>{/if}
  {#if !items.length}
    <p class="muted">{t('journal.empty')}</p>
  {:else}
    <ul class="jlist">
      {#each items as ev (ev.id)}
        <li bind:this={rows[ev.id]} class:mark={ev.id === mark} aria-current={ev.id === mark ? 'true' : undefined}>
          <i class="dot {SEV[ev.severity] === 'ok' ? eventKind(ev) : SEV[ev.severity] || 'ok'}"></i>
          <span class="jt">{eventText(ev)}{#if ev.subject}<small class="muted"> · {ev.subject}</small>{/if}</span>
          <small class="muted">{time(ev.time)}</small>
        </li>
      {/each}
    </ul>
    {#if more}<button onclick={() => load(true)}>{t('common.showMore')}</button>{/if}
  {/if}
</section>

<style>
  .jlist { list-style: none; margin: 0; padding: 0; display: flex; flex-direction: column; gap: 4px; }
  .jlist li { display: grid; grid-template-columns: auto 1fr auto; gap: 10px; align-items: center; padding: 6px 10px; border-radius: 8px; border: 1px solid transparent; }
  .jlist li.mark { border-color: var(--accent); background: var(--surface2); box-shadow: 0 0 0 2px var(--accent); }
  .jt { min-width: 0; overflow-wrap: anywhere; }
  .dot { width: 8px; height: 8px; border-radius: 50%; background: var(--ok); }
  .dot.warn { background: var(--warn); }
  .dot.bad { background: var(--bad); }
  @media (max-width: 600px) { .jlist li { grid-template-columns: auto 1fr; } .jlist li small:last-child { grid-column: 2; } }
</style>
