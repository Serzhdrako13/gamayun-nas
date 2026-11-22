<script>
  // Установка приложения из каталога — форма x-gamayun.form →
  // «Проверить» (совместимость, сверка security, риски, порты) → задача
  // установки с откатом при сбое.
  import { untrack, onMount } from 'svelte';
  import { flash } from './state.svelte.js';
  import { get, post } from './api.js';
  import Drawer from './Drawer.svelte';
  import CheckReport from './CheckReport.svelte';
  import HelpButton from '../help/HelpButton.svelte';
  import { ready, validId, waitTask } from './apps.js';
  import { textOf, visibleField, initialValues, formValues, formFilled, exposable } from './catalog.js';
  import { t, lang } from './i18n/index.js';

  let { entry, onclose } = $props();
  const m = untrack(() => entry.manifest);
  let id = $state(m.id);
  let values = $state(initialValues(m.form));
  let network = $state(new Set());
  let shares = $state([]);
  let check = $state(null);
  let checkedAt = $state('');
  let confirm = $state(new Set());
  let err = $state('');
  let busy = $state(false);
  const ports = exposable(m);
  const fields = $derived((m.form || []).filter((f) => visibleField(f, values)));
  const body = () => ({ id, values: formValues(m.form, values), network: [...network].sort(), confirm: [...confirm] });
  const snapshot = () => JSON.stringify([id, formValues(m.form, values), [...network].sort()]);
  const stale = $derived(!!check && checkedAt !== snapshot());
  const canInstall = $derived(!busy && !stale && ready(check, confirm) && check?.ok !== undefined);

  onMount(async () => {
    if (!(m.form || []).some((f) => f.type === 'share')) return;
    try {
      shares = (await get('/nas/shares')).shares.map((s) => s.name).sort();
    } catch (e) {
      err = e.message;
    }
  });

  async function doCheck() {
    err = '';
    busy = true;
    try {
      check = await post(`/apps/catalog/${encodeURIComponent(m.id)}/check`, body());
      checkedAt = snapshot();
    } catch (e) {
      err = e.message;
      check = null;
    }
    busy = false;
  }

  async function install() {
    err = '';
    busy = true;
    try {
      flash(t('apps.catalog.installing'));
      const tk = await waitTask(get, await post(`/apps/catalog/${encodeURIComponent(m.id)}/install`, body()));
      if (tk.state === 'succeeded') {
        flash(t('apps.catalog.installed1', { id }));
        onclose();
      } else if (tk.state === 'failed') err = t('apps.taskFailed', { error: tk.error?.message || '' });
      else {
        flash(t('apps.taskRunning'));
        onclose();
      }
    } catch (e) {
      err = e.message;
    }
    busy = false;
  }

  const set = (k, v) => (values = { ...values, [k]: v });
  function toggle(set0, key, on) {
    const s = new Set(set0);
    on ? s.add(key) : s.delete(key);
    return s;
  }
</script>

<Drawer title={t('apps.catalog.installTitle', { name: textOf(m.name, lang()) })} {onclose}>
  <div class="stack">
    <p class="small muted">{textOf(m.description, lang())} <HelpButton section="apps.catalog" /></p>
    <label class="field"><span>{t('apps.catalog.id')}</span>
      <input bind:value={id} autocomplete="off" aria-invalid={!validId(id)} />
      <span class="small muted">{t('apps.editor.idHint')}</span></label>
    {#each fields as f (f.key)}
      {@const title = textOf(f.title, lang()) + (f.required ? ' *' : '')}
      {#if f.type === 'random'}
        <p class="small"><b>{title}</b> — <span class="muted">{t('apps.catalog.random')}</span></p>
      {:else if f.type === 'bool'}
        <label class="row"><input type="checkbox" checked={values[f.key] === 'true'} onchange={(e) => set(f.key, e.currentTarget.checked ? 'true' : 'false')} /> {title}</label>
      {:else if f.type === 'select' || f.type === 'share'}
        <label class="field"><span>{title}</span>
          <select value={values[f.key]} onchange={(e) => set(f.key, e.currentTarget.value)}>
            <option value="">{t('apps.catalog.choose')}</option>
            {#each f.type === 'share' ? shares : f.options || [] as o}<option value={o}>{o}</option>{/each}
          </select></label>
      {:else}
        <label class="field"><span>{title}</span>
          <input type={f.type === 'password' ? 'password' : f.type === 'int' ? 'number' : 'text'} autocomplete="off"
            value={values[f.key]} oninput={(e) => set(f.key, e.currentTarget.value)} /></label>
      {/if}
    {/each}
    {#if ports.length}
      <div class="stack"><b>{t('apps.catalog.network')}</b>
        <p class="small muted">{t('apps.catalog.networkHint')}</p>
        {#each ports as p}
          <label class="row"><input type="checkbox" checked={network.has(p.key)} onchange={(e) => (network = toggle(network, p.key, e.currentTarget.checked))} />
            {textOf(p.title, lang())} — {p.key}</label>
        {/each}</div>
    {/if}
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <div class="row">
      <button disabled={busy || !validId(id) || !formFilled(m.form, values)} onclick={doCheck}>{t('apps.catalog.check')}</button>
      {#if stale}<span class="small muted" role="status">{t('apps.catalog.recheck')}</span>{/if}
    </div>
    {#if check}
      <CheckReport {check} {confirm} ontoggle={(k, on) => (confirm = toggle(confirm, k, on))} />
      {#if ready(check, confirm) && !stale}<p class="small" role="status">{t('apps.catalog.ok')}</p>{/if}
    {/if}
    <div class="row">
      <button class="primary" disabled={!canInstall} onclick={install}>{t('apps.catalog.install')}</button>
      <button onclick={onclose}>{t('common.cancel')}</button>
    </div>
  </div>
</Drawer>
