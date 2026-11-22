<script>
  import Switch from './Switch.svelte';
  import { onMount } from 'svelte';
  import Drawer from './Drawer.svelte';
  import { app, refresh, flash } from './state.svelte.js';
  import { get, post } from './api.js';
  import { bytes, POOL_TYPES, freeDisks, failures } from './format.js';
  import { t, locale } from './i18n/index.js';
  import HelpButton from '../help/HelpButton.svelte';

  let { onclose } = $props();
  let step = $state(1);
  let snap = $state(null);
  let name = $state('');
  let type = $state('shr');
  let integrity = $state(true);
  let picked = $state([]);
  let preview = $state(null);
  let previewErr = $state('');
  let plan = $state(null);
  let err = $state('');
  let busy = $state(false);
  let taskId = $state('');

  onMount(async () => {
    try {
      snap = await get('/disks');
    } catch (e) {
      err = e.message;
    }
  });
  const free = $derived(freeDisks(snap));
  const chosen = $derived(free.filter((d) => picked.includes(d.id)));
  const nameOk = $derived(/^[a-z][a-z0-9]{0,15}$/.test(name) && !app.pools.some((p) => p.name === name));
  const task = $derived(app.tasks.find((x) => x.id === taskId));

  // Живой расчёт планировщика при любом изменении выбора: ничего не пишет.
  let seq = 0;
  $effect(() => {
    const ty = type;
    const ds = chosen.map((d) => ({ id: d.id, size_bytes: d.size_bytes }));
    preview = null;
    previewErr = '';
    if (!ds.length) return;
    const my = ++seq;
    post('/storage/plan', { type: ty, disks: ds })
      .then((p) => my === seq && (preview = p))
      .catch((e) => my === seq && (previewErr = e.message));
  });

  function toggle(id) {
    picked = picked.includes(id) ? picked.filter((x) => x !== id) : [...picked, id];
  }

  async function makePlan() {
    busy = true;
    err = '';
    try {
      plan = await post('/storage/pools/plans', { op: 'create', pool: name, type, disk_ids: picked, integrity });
      step = 3;
    } catch (e) {
      err = e.message;
    } finally {
      busy = false;
    }
  }

  async function confirm() {
    busy = true;
    err = '';
    try {
      const r = await post(`/storage/pools/plans/${encodeURIComponent(plan.plan_id)}/confirm`, { confirm_token: plan.confirm_token });
      taskId = r.task.id;
      plan = null;
      step = 4;
      flash(t('poolWizard.started'));
      refresh();
    } catch (e) {
      err = e.message;
    } finally {
      busy = false;
    }
  }
</script>

<Drawer title={t('poolWizard.title', { step: Math.min(step, 3) })} {onclose}>
  <div class="stack">
    {#if step === 1}
      <label class="field"><span>{t('poolWizard.nameLabel')}</span><input bind:value={name} placeholder="home" /></label>
      {#if name && !nameOk}<p class="error">{t('poolWizard.nameBad')}</p>{/if}
      <label class="field">
        <span>{t('poolWizard.scheme')} <HelpButton section="storage.pools.raid" /></span>
        <select bind:value={type}>
          {#each Object.entries(POOL_TYPES) as [k, v]}<option value={k}>{v}</option>{/each}
        </select>
      </label>
      <Switch label={t('poolWizard.integrity')} hint={t('poolWizard.integrityHint')} checked={integrity} onchange={(v) => (integrity = v)} />

      <h2>{t('poolWizard.freeDisks')}</h2>
      {#if !snap}<p class="muted">{t('common.loading')}</p>
      {:else if !snap.complete}<p class="banner warn">{t('poolWizard.incomplete')}</p>
      {:else if !free.length}<p class="muted">{t('poolWizard.noFree')}</p>{/if}
      {#each free as d}
        <label class="row"><input type="checkbox" checked={picked.includes(d.id)} onchange={() => toggle(d.id)} /> <span>{d.name} — {bytes(d.size_bytes)} {d.kind}{d.model ? ', ' + d.model : ''}</span></label>
      {/each}

      <section class="card" aria-live="polite">
        <h2>{t('poolWizard.result')}</h2>
        {#if !picked.length}<p class="muted">{t('poolWizard.pickDisks')}</p>
        {:else if previewErr}<p class="error">{previewErr}</p>
        {:else if preview}
          <p>{t('poolWizard.usable')}<b>{bytes(preview.usable_bytes)}</b>{t('poolWizard.usableOf', { raw: bytes(preview.raw_bytes), unused: bytes(preview.unused_bytes) })}</p>
          <p>{t('poolWizard.tolerance')}<b>{failures(preview.tolerance)}</b>.</p>
          {#each preview.warnings || [] as w}<p class="small" style:color="var(--warn)">{w}</p>{/each}
          {#if preview.transitions?.length}
            <p class="small muted">{t('poolWizard.growLater')}</p>
            <ul class="small">
              {#each preview.transitions as tr}<li>{tr.note || tr.op + ' → ' + tr.target}{tr.verified ? '' : t('poolWizard.notVerified')}</li>{/each}
            </ul>
          {/if}
        {:else}<p class="muted">{t('poolWizard.calculating')}</p>{/if}
      </section>
      {#if err}<p class="error">{err}</p>{/if}
      <button class="primary" disabled={!nameOk || !preview || busy} onclick={() => (step = 2)}>{t('poolWizard.next')}</button>
    {:else if step === 2}
      <p>{t('poolWizard.review.before')}<b>{t('poolWizard.review.wiped')}</b>{t('poolWizard.review.after')}</p>
      <ul>
        <li>{t('poolWizard.review.pool')}<b>{name}</b>, {POOL_TYPES[type]}{integrity ? t('poolWizard.review.withIntegrity') : ''}</li>
        <li>{t('poolWizard.review.disks')}{chosen.map((d) => d.name + ' (' + bytes(d.size_bytes) + ')').join(', ')}</li>
        <li>{t('poolWizard.review.capacity')}{bytes(preview?.usable_bytes)}, {failures(preview?.tolerance ?? 0)}</li>
      </ul>
      {#if err}<p class="error">{err}</p>{/if}
      <div class="row"><button onclick={() => (step = 1)}>{t('common.back')}</button><button class="primary" disabled={busy} onclick={makePlan}>{t('poolWizard.makePlan')}</button></div>
    {:else if step === 3 && plan}
      <p>{t('poolWizard.plan.valid', { time: new Date(plan.expires_at).toLocaleTimeString(locale()) })}</p>
      <ul>
        <li>{t('poolWizard.plan.capacity')}<b>{bytes(plan.usable_bytes)}</b>{t('poolWizard.plan.overhead')}{failures(plan.tolerance)}</li>
        <li>{t('poolWizard.plan.wiped')}{plan.disks.map((d) => d.name + ' (' + bytes(d.size_bytes) + ')').join(', ')}</li>
      </ul>
      {#each plan.warnings || [] as w}<p class="banner warn">{w}</p>{/each}
      {#if err}<p class="error">{err}</p>{/if}
      <div class="row"><button onclick={() => (step = 2)}>{t('common.back')}</button><button class="danger" disabled={busy} onclick={confirm}>{t('poolWizard.plan.confirm')}</button></div>
    {:else}
      <h2>{t('poolWizard.creating', { name })}</h2>
      {#if task}
        <p>{['queued', 'running', 'succeeded', 'failed', 'interrupted'].includes(task.state) ? t('poolWizard.taskState.' + task.state) : task.state}, {task.progress}%</p>
        <div class="bar"><i style:width={task.progress + '%'}></i></div>
        {#if task.error}<p class="error">{task.error.message}</p>{/if}
        {#if task.state === 'succeeded'}<p>{t('poolWizard.doneHint')}</p>{/if}
      {:else}<p class="muted">{t('poolWizard.waitTask')}</p>{/if}
      <button onclick={onclose}>{t('common.close')}</button>
    {/if}
  </div>
</Drawer>
