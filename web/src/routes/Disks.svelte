<script>
  import { onMount } from 'svelte';
  import { get, post, put } from '../lib/api.js';
  import { flash } from '../lib/state.svelte.js';
  import { bytes, diskState } from '../lib/format.js';
  import Drawer from '../lib/Drawer.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import SmartChart from '../lib/SmartChart.svelte';
  import { smartBadge } from '../lib/smart.js';
  import { t } from '../lib/i18n/index.js';

  let snap = $state(null);
  let err = $state('');
  let q = $state('');
  let sel = $state(null);
  let sm = $state(null);
  let smErr = $state('');

  // SMART выбранного диска — свежий опрос.
  async function openDisk(d) {
    sel = d;
    sm = null;
    smErr = '';
    if (!d.id) return;
    try {
      sm = await get(`/disks/${encodeURIComponent(d.id)}/smart`);
    } catch (e) {
      smErr = e.message;
    }
  }
  async function selfTest(type) {
    smErr = '';
    try {
      await post(`/disks/${encodeURIComponent(sel.id)}/smart/test`, { type });
      flash(type === 'long' ? t('disks.longTestStarted') : t('disks.shortTestStarted'));
    } catch (e) {
      smErr = e.message;
    }
  }
  // Включить/выключить SMART в диске вручную.
  async function setSmart(on) {
    smErr = '';
    try {
      sm = await put(`/disks/${encodeURIComponent(sel.id)}/smart/enabled`, { on });
      flash(on ? t('disks.smart.enabled') : t('disks.smart.disabledFlash'));
      load();
    } catch (e) {
      smErr = e.message;
    }
  }
  const n = (v) => (v === undefined || v === null ? '—' : v);

  async function load() {
    try {
      snap = await get('/disks');
      err = '';
    } catch (e) {
      err = e.message;
    }
  }
  onMount(() => {
    load();
    const t = setInterval(() => document.visibilityState === 'visible' && load(), 15000);
    return () => clearInterval(t);
  });
  const rows = $derived((snap?.disks || []).filter((d) => [d.name, d.model, d.id, d.kind].join(' ').toLowerCase().includes(q.toLowerCase())));
  const ROLES = $derived({ root: t('disks.roles.root'), boot: t('disks.roles.boot'), efi: 'EFI', swap: 'swap', mounted: t('disks.roles.mounted'), stack_member: t('disks.roles.stackMember') });
</script>

{#if err}<p class="error">{err}</p>{/if}
{#if snap && !snap.complete}
  <div class="banner warn">{t('disks.incomplete')}</div>
{/if}
{#each snap?.issues || [] as i}<p class="small muted">{i.kind} {i.mountpoint || i.source}: {i.reason}</p>{/each}
<div class="stack">
  <input placeholder={t('disks.filter')} bind:value={q} />
  <div class="card tablewrap">
    <table>
      <thead><tr><th>{t('disks.col.disk')}</th><th>{t('disks.col.type')}</th><th>{t('common.capacity')}</th><th>{t('common.model')}</th><th>{t('common.state')}</th><th title={t('disks.col.smartHint')}>SMART</th></tr></thead>
      <tbody>
        {#each rows as d}
          {@const s = diskState(d)}
          <tr>
            <td><button class="link" onclick={() => openDisk(d)}>{d.name}</button></td>
            <td>{d.kind}</td>
            <td>{bytes(d.size_bytes)}</td>
            <td>{d.model || '—'}</td>
            <td><span class="badge {s[1]}">{s[0]}</span></td>
            {#if d.id}
              {@const b = smartBadge(d.smart)}
              <td><span class="badge {b[1]}" title={d.smart?.reasons?.join('; ') || ''}>{b[0]}</span></td>
            {:else}<td class="muted">—</td>{/if}
          </tr>
        {:else}
          <tr><td colspan="6" class="muted">{snap ? t('disks.none') : t('common.loading')}</td></tr>
        {/each}
      </tbody>
    </table>
  </div>
</div>

{#if sel}
  <Drawer title={sel.name} onclose={() => (sel = null)}>
    <dl class="stack">
      <div><dt class="muted small">{t('disks.id')}</dt><dd class="mono">{sel.id || t('disks.noId')}</dd></div>
      <div><dt class="muted small">{t('disks.kernelName')}</dt><dd class="mono">{sel.kernel_name}</dd></div>
      <div><dt class="muted small">{t('common.capacity')}</dt><dd>{bytes(sel.size_bytes)} ({sel.kind})</dd></div>
      <div><dt class="muted small">{t('common.model')}</dt><dd>{sel.model || '—'}</dd></div>
      <div><dt class="muted small">{t('disks.roles.title')}</dt><dd>{sel.system_roles?.length ? sel.system_roles.map((r) => ROLES[r] || r).join(', ') : t('disks.roles.none')}</dd></div>
      {#if sel.unknown_reason}<div><dt class="muted small">{t('disks.unknownReason')}</dt><dd>{sel.unknown_reason}</dd></div>{/if}
    </dl>
    <h3>SMART</h3>
    {#if smErr}<p class="error">{smErr}</p>{/if}
    {#if !sel.id}
      <p class="small muted">{t('disks.smart.noId')}</p>
    {:else if !sm}
      {#if !smErr}<p class="small muted">{t('disks.smart.polling')}</p>{/if}
    {:else if sm.standby}
      <p class="small muted">{t('disks.smart.standby')}</p>
    {:else if sm.disabled}
      {#if sm.manual_off}
        <p class="small muted">{t('disks.smart.manualOff')}</p>
      {:else}
        <p class="banner warn">{t('disks.smart.autoEnableFailed')}</p>
      {/if}
      <div class="row"><button onclick={() => setSmart(true)}>{t('disks.smart.enable')}</button></div>
    {:else if !sm.supported}
      <p class="small muted">{t('disks.smart.unsupported')}</p>
    {:else}
      {@const lim = sel.smart?.limits}
      <dl class="stack">
        <div><dt class="muted small">{t('disks.smart.assessment')}</dt><dd>{#if sm.passed === false}<span class="badge bad">{t('disks.smart.failing')}</span>{:else if sm.passed}<span class="badge ok">{t('common.inNorm')}</span>{:else}—{/if}</dd></div>
        <div><dt class="muted small">{t('disks.smart.tempHours')}</dt><dd>{sm.temperature_c ? sm.temperature_c + ' °C' : '—'}{#if lim && sm.temperature_c >= lim.warning_c} <span class="badge {sm.temperature_c >= lim.critical_c ? 'bad' : 'warn'}">{t('disks.smart.above', { n: sm.temperature_c >= lim.critical_c ? lim.critical_c : lim.warning_c })}</span>{/if} / {sm.power_on_hours ? t('disks.smart.hours', { n: sm.power_on_hours }) : '—'}</dd></div>
        <div><dt class="muted small">{t('disks.smart.sectors')}</dt><dd>{n(sm.reallocated)} / {n(sm.pending)} / {n(sm.uncorrectable)}</dd></div>
        <div><dt class="muted small">{t('disks.smart.selfTest')}</dt><dd>{#if sm.test_running}{t('disks.smart.testRunning')}{sm.test_remaining != null ? t('disks.smart.testRemaining', { n: sm.test_remaining }) : ''}{:else if sm.last_test}{sm.last_test.type === 'long' ? t('disks.smart.long') : t('disks.smart.short')}: {sm.last_test.passed ? t('disks.smart.noErrors') : sm.last_test.status}{:else}{t('disks.smart.neverRun')}{/if}</dd></div>
      </dl>
      <div class="row">
        <button onclick={() => selfTest('short')} disabled={sm.test_running}>{t('disks.smart.shortTest')}</button>
        <button onclick={() => selfTest('long')} disabled={sm.test_running}>{t('disks.smart.longTest')}</button>
        <Confirm label={t('disks.smart.disable')} onconfirm={() => setSmart(false)} hint={t('disks.smart.disableHint')} />
      </div>
    {/if}
    {#if sel.id && sm?.supported}<SmartChart diskId={sel.id} limits={sel.smart?.limits} />{/if}
  </Drawer>
{/if}
