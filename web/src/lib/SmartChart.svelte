<script>
  // История температуры и плохих секторов диска — свой SVG, цвета из темы.
  import { get } from './api.js';
  import Seg from './Seg.svelte';
  import { series, HISTORY_DAYS } from './smart.js';
  import { t, locale } from './i18n/index.js';

  let { diskId, limits = null } = $props();
  let days = $state(7);
  let pts = $state(null);
  let err = $state('');
  let now = $state(Date.now());

  $effect(() => {
    const id = diskId, d = days;
    pts = null;
    err = '';
    get(`/disks/${encodeURIComponent(id)}/smart/history?days=${d}`)
      .then((r) => { if (id === diskId && d === days) { pts = r.points; now = Date.now(); } })
      .catch((e) => (err = e.message));
  });

  const W = 320, H = 110, SH = 44;
  const from = $derived(now - days * 864e5);
  // Разрыв линии: нет точек дольше 3 плановых опросов (неделя) или 3 корзин прореживания.
  const gap = $derived(days > 7 ? 18 * 36e5 : 2 * 36e5);
  const temp = $derived(series(pts, 'temperature_c', { from, to: now, w: W, h: H, lo: 20, hi: limits ? limits.critical_c + 2 : 60, gap, pad: 4 }));
  const SECT = [['reallocated'], ['pending'], ['uncorrectable']];
  const sectMax = $derived(Math.max(0, ...(pts || []).flatMap((p) => SECT.map(([k]) => p[k] || 0))));
  const sect = $derived(SECT.map(([k]) => ({ k, label: t('smart.sectors.' + k), ...series(pts, k, { from, to: now, w: W, h: SH, lo: 0, hi: Math.max(1, sectMax), gap, pad: 3 }) })));
  const last = $derived(pts?.length ? pts[pts.length - 1] : null);
  const tmax = $derived(Math.max(...(pts || []).map((p) => p.temperature_c ?? -Infinity)));
  const date = (t) => new Date(t).toLocaleString(locale(), days > 1 ? { day: 'numeric', month: 'short' } : { hour: '2-digit', minute: '2-digit' });
</script>

<div class="row between">
  <h4>{t('smart.chart.history')}</h4>
  <Seg label={t('smart.chart.period')} options={HISTORY_DAYS.map((d) => [d, d === 1 ? t('smart.chart.day') : t('smart.chart.days', { n: d })])} value={days} onchange={(v) => (days = v)} />
</div>
{#if err}<p class="error">{err}</p>
{:else if !pts}<p class="small muted">{t('common.loading')}</p>
{:else if !pts.length}<p class="small muted">{t('smart.chart.noPoints')}</p>
{:else}
  <figure class="smart-g">
    <figcaption class="small muted">{t('smart.chart.temp')}{isFinite(tmax) ? t('smart.chart.tempMax', { v: tmax }) : ''}{last?.temperature_c != null ? t('smart.chart.tempNow', { v: last.temperature_c }) : ''}</figcaption>
    <svg viewBox="0 0 {W} {H}" preserveAspectRatio="none" role="img" aria-label={t('smart.chart.tempAria', { n: days })}>
      {#if limits}
        {#each [['warn', limits.warning_c], ['bad', limits.critical_c]] as [k, v]}
          {#if v <= temp.hi}<line class="lim {k}" x1="0" x2={W} y1={temp.y(v)} y2={temp.y(v)} />{/if}
        {/each}
      {/if}
      {#each temp.lines as l}<polyline class="t" points={l} />{/each}
    </svg>
    <div class="smart-ax small muted"><span>{date(from)}</span><span>{temp.lo}–{temp.hi} °C{limits ? t('smart.chart.limits', { warn: limits.warning_c, crit: limits.critical_c }) : ''}</span><span>{date(now)}</span></div>
  </figure>
  <figure class="smart-g">
    <figcaption class="small muted">{t('smart.chart.badSectors')}{sectMax ? '' : t('smart.chart.badSectorsZero')}</figcaption>
    {#if sectMax}
      <svg class="sect-g" viewBox="0 0 {W} {SH}" preserveAspectRatio="none" role="img" aria-label={t('smart.chart.sectAria')}>
        {#each sect as s}{#each s.lines as l}<polyline class="s {s.k}" points={l} />{/each}{/each}
      </svg>
      <div class="smart-ax small muted">{#each sect as s}<span><i class="sw {s.k}"></i>{s.label}: {last?.[s.k] ?? '—'}</span>{/each}</div>
    {/if}
  </figure>
{/if}

<style>
  .smart-g { margin: 8px 0; }
  .smart-g svg { display: block; width: 100%; height: 110px; background: var(--surface2); border-radius: 8px; }
  .smart-g svg.sect-g { height: 44px; } /* было [aria-label^=…]: подпись теперь зависит от языка */
  polyline { fill: none; stroke-width: 1.8; vector-effect: non-scaling-stroke; stroke-linejoin: round; }
  .t { stroke: var(--accent2); }
  .lim { stroke-width: 1; stroke-dasharray: 4 3; vector-effect: non-scaling-stroke; }
  .lim.warn { stroke: var(--warn); } .lim.bad { stroke: var(--bad); }
  .s.reallocated, .sw.reallocated { stroke: var(--warn); background: var(--warn); }
  .s.pending, .sw.pending { stroke: var(--accent2); background: var(--accent2); }
  .s.uncorrectable, .sw.uncorrectable { stroke: var(--bad); background: var(--bad); }
  .smart-ax { display: flex; justify-content: space-between; gap: 8px; flex-wrap: wrap; margin-top: 2px; }
  .sw { display: inline-block; width: 10px; height: 3px; margin-right: 4px; vertical-align: middle; }
  h4 { margin: 0; }
</style>
