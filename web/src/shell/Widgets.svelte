<script>
  // Колонка виджетов: часы, система, хранилище, сеть, события.
  // Живые данные: /system/metrics и /events/recent раз в 5 с, пока вкладка видна;
  // пулы — из общего состояния (опрос state.svelte.js).
  import { onMount } from 'svelte';
  import { app } from '../lib/state.svelte.js';
  import { get } from '../lib/api.js';
  import { bytes, health } from '../lib/format.js';
  import Widget from '../ui/Widget.svelte';
  import Ring from '../ui/Ring.svelte';
  import Meter from '../ui/Meter.svelte';
  import Status from '../ui/Status.svelte';
  import { eventText, eventKind } from './events.js';
  import { smartCounts } from '../lib/smart.js';
  import { mbps, rates, push, line } from './net.js';
  import { t, locale } from '../lib/i18n/index.js';

  let { onopen = () => {} } = $props();
  let now = $state(new Date());
  let m = $state(null);
  let evs = $state([]);
  // Адаптер виджета сети ('' — все), выбор — в браузере.
  const NKEY = 'dn.net.iface';
  let iface = $state((() => { try { return localStorage.getItem(NKEY) || ''; } catch { return ''; } })());
  $effect(() => { try { localStorage.setItem(NKEY, iface); } catch { /* приватный режим */ } });
  let hist = $state([]); // снимки метрик для графика
  // SMART дисков — итог последнего планового опроса, раз в минуту.
  let dsk = $state(null);
  async function pollDisks() {
    if (document.hidden) return;
    try { dsk = smartCounts((await get('/disks')).disks); } catch { /* показываем прошлое */ }
  }

  async function poll() {
    if (document.hidden) return;
    try { m = await get('/system/metrics'); hist = push(hist, m); } catch { /* показываем прошлое */ }
    try { evs = (await get('/events/recent?limit=3')).events; } catch { /* нет журнала */ }
  }
  onMount(() => {
    poll();
    pollDisks();
    const a = setInterval(poll, 5000), b = setInterval(() => (now = new Date()), 10000), c = setInterval(pollDisks, 60000);
    return () => { clearInterval(a); clearInterval(b); clearInterval(c); };
  });
  const hm = $derived(now.toLocaleTimeString(locale(), { hour: '2-digit', minute: '2-digit' }));
  const day = $derived(now.toLocaleDateString(locale(), { weekday: 'long', day: 'numeric', month: 'long' }));
  const mem = $derived(m && m.mem_total_bytes ? (m.mem_used_bytes / m.mem_total_bytes) * 100 : 0);
  const ifaces = $derived(m?.net_ifaces || []);
  const cur = $derived(rates(m, iface));
  const series = $derived(hist.map((x) => rates(x, iface)));
  const top = $derived(Math.max(0, ...series.map((x) => Math.max(x.rx || 0, x.tx || 0))));
  const W = 240, H = 36;
  // Клик по событию — журнал с подсветкой этого события.
  function openEvent(id) {
    const h = '#/journal?event=' + id;
    if (location.hash === h) dispatchEvent(new HashChangeEvent('hashchange'));
    else location.hash = h;
  }
</script>

<aside class="sh-widgets">
  <Widget><div class="sh-clock">{hm}</div><div class="muted">{day}</div></Widget>
  <Widget title={t('widgets.system')}>
    <div class="sh-rings">
      <Ring value={m?.cpu_percent ?? 0} text={m?.cpu_percent == null ? '—' : Math.round(m.cpu_percent) + '%'} label={t('widgets.cpu')} />
      <Ring value={mem} text={m ? Math.round(mem) + '%' : '—'} label={t('widgets.ram')} />
    </div>
    <div class="small muted">{m ? t('widgets.cpuMem', { count: m.cpu_count, used: bytes(m.mem_used_bytes), total: bytes(m.mem_total_bytes) }) : t('common.loadingLower')}</div>
  </Widget>
  <Widget title={t('widgets.storage')} onclick={() => onopen('storage')}>
    {#each app.pools as p (p.name)}
      {@const [htxt, hk] = health(p)}
      {@const u = p.status?.btrfs}
      <div class="sh-pool"><b>{p.name}</b><Status kind={hk === 'unknown' ? 'free' : hk}>{htxt}</Status></div>
      {#if u}<Meter value={u.used} max={u.device_size} /><div class="small muted">{t('widgets.usedOf', { used: bytes(u.used), total: bytes(u.device_size) })}</div>{/if}
    {:else}
      <div class="muted">{t('widgets.noPools')}</div>
    {/each}
  </Widget>
  <Widget title={t('widgets.disks')} onclick={() => onopen('storage')}>
    {#if !dsk}
      <div class="muted">{t('common.loadingLower')}</div>
    {:else}
      <div class="sh-pool"><b>SMART</b>
        {#if dsk.critical}<Status kind="bad">{t('widgets.smartCritical', { count: dsk.critical })}</Status>
        {:else if dsk.warning}<Status kind="warn">{t('widgets.smartWarning', { count: dsk.warning })}</Status>
        {:else if dsk.ok}<Status kind="ok">{t('common.inNorm')}</Status>
        {:else}<Status kind="free">{t('widgets.noData')}</Status>{/if}
      </div>
      {#each dsk.problems.slice(0, 3) as p}<div class="small">{p.name}: {p.reasons.join('; ')}</div>{/each}
      <div class="small muted">{t('widgets.withSmart', { count: dsk.ok + dsk.warning + dsk.critical })}{dsk.unknown ? t('widgets.withoutData', { count: dsk.unknown }) : ''}{dsk.maxTemp != null ? t('widgets.maxTemp', { temp: dsk.maxTemp }) : ''}</div>
    {/if}
  </Widget>
  <Widget title={t('widgets.network')}>
    {#snippet extra()}
      <select class="sh-net-sel" bind:value={iface} aria-label={t('widgets.iface')}>
        <option value="">{t('widgets.allIfaces')}</option>
        {#each ifaces as n (n)}<option value={n}>{n}</option>{/each}
        {#if iface && !ifaces.includes(iface)}<option value={iface}>{t('widgets.ifaceGone', { name: iface })}</option>{/if}
      </select>
    {/snippet}
    <div class="sh-net">
      <span class="rx" title={t('widgets.rx')}><i aria-hidden="true">↓</i> {mbps(cur.rx)}</span>
      <span class="tx" title={t('widgets.tx')}><i aria-hidden="true">↑</i> {mbps(cur.tx)}</span>
    </div>
    <svg class="sh-net-g" viewBox="0 0 {W} {H}" preserveAspectRatio="none" role="img" aria-label={t('widgets.chart')}>
      <polyline class="rx" points={line(series.map((x) => x.rx), top, W, H)} />
      <polyline class="tx" points={line(series.map((x) => x.tx), top, W, H)} />
    </svg>
    <div class="small muted">{t('widgets.peak', { rate: mbps(top || null) })}</div>
  </Widget>
  <Widget title={t('widgets.events')} onhead={() => onopen('journal')}>
    {#each evs as ev (ev.id)}
      <button class="sh-ev" onclick={() => openEvent(ev.id)} title={t('widgets.openInJournal')}><i class={eventKind(ev)}></i><span>{eventText(ev)}</span></button>
    {:else}
      <div class="muted">{t('widgets.noEvents')}</div>
    {/each}
  </Widget>
</aside>
