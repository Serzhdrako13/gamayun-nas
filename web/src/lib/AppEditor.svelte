<script>
  // Мастер «Добавить/изменить приложение»: compose + .env →
  // «Проверить» (нормализация, запреты, риски, конфликты портов) →
  // подтверждение рисков → создание задачей с откатом при сбое.
  import { untrack } from 'svelte';
  import { flash } from './state.svelte.js';
  import { get, post, put } from './api.js';
  import Drawer from './Drawer.svelte';
  import HelpButton from '../help/HelpButton.svelte';
  import { ready, requestBody, validId, waitTask, portKey, shareRows, fillGrants, usedGrants } from './apps.js';
  import { t } from './i18n/index.js';

  // migrate — перевод приложения «доступ как root» на свою учётку.
  let { edit = null, migrate = false, onclose } = $props();
  const init = untrack(() => edit); // мастер открывается заново на каждое приложение
  const mig = untrack(() => migrate);
  // своя учётка: новое, уже со своей учёткой или перевод; «доступ как root» без учётки — как раньше
  const own = !init || init.uid > 0 || mig;
  let runAs = $state({ ...(init?.run_as || {}) });
  let grants = $state(Object.fromEntries((init?.grants || []).map((g) => [g.share, g.mode])));
  let f = $state({ id: init?.id || '', name: init?.name || '', compose: init?.compose || '', env: '', keepEnv: !!init });
  let check = $state(null);
  let checkedAt = $state(''); // снимок полей, по которым проверено
  let confirm = $state(new Set());
  let network = $state(new Set(init?.network || []));
  let err = $state('');
  let busy = $state(false);
  const access = () => (own ? { runAs, grants, migrate: mig } : null);
  const snapshot = () => JSON.stringify([f.id, f.name, f.compose, f.keepEnv ? null : f.env, [...network].sort(), own ? [runAs, grants] : null]);
  const rows = $derived(shareRows(check));
  const stale = $derived(!!check && checkedAt !== snapshot());
  const canSave = $derived(!busy && !stale && ready(check, confirm) && f.name.trim() !== '');

  async function doCheck() {
    err = '';
    busy = true;
    try {
      check = await post('/apps/projects/check', requestBody(f, null, confirm, network, access()));
      if (own) {
        // Папкам без выдачи — «только чтение» (при переводе — как было) и повторная проверка.
        const fg = fillGrants(usedGrants(grants, check), check, mig);
        if (fg.added || Object.keys(fg.grants).length !== Object.keys(grants).length) {
          grants = fg.grants;
          check = await post('/apps/projects/check', requestBody(f, null, confirm, network, access()));
        }
      }
      checkedAt = snapshot();
    } catch (e) {
      err = e.message;
      check = null;
    }
    busy = false;
  }

  function toggle(set, key, on) {
    const s = new Set(set);
    on ? s.add(key) : s.delete(key);
    return s;
  }

  async function save() {
    err = '';
    busy = true;
    try {
      const body = requestBody(f, check, confirm, network, access());
      const path = `/apps/projects/${encodeURIComponent(f.id)}`;
      const tk0 = mig
        ? await post(`${path}/migrate`, { run_as: body.run_as, grants: body.grants, confirm: body.confirm })
        : edit ? await put(path, body) : await post('/apps/projects', body);
      const tk = await waitTask(get, tk0);
      if (tk.state === 'succeeded') {
        flash(t(mig ? 'apps.done.migrate' : edit ? 'apps.done.update' : 'apps.done.create', { id: f.id }));
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

  async function loadFile(e) {
    const file = e.currentTarget.files?.[0];
    if (file) f.compose = await file.text();
  }
</script>

<Drawer title={mig ? t('apps.access.migrateTitle', { id: f.id }) : edit ? t('apps.editor.titleEdit', { id: f.id }) : t('apps.editor.titleNew')} {onclose}>
  <div class="stack">
    {#if mig}<p class="banner warn">{t('apps.access.migrateAbout')} <HelpButton section="apps.access" /></p>{/if}
    <p class="small muted">{t('apps.userHint')} <HelpButton section="apps.add" /></p>
    <label class="field"><span>{t('apps.editor.id')}</span>
      <input bind:value={f.id} disabled={!!edit} autocomplete="off" aria-invalid={f.id !== '' && !validId(f.id)} />
      <span class="small muted">{t('apps.editor.idHint')}</span></label>
    <label class="field"><span>{t('apps.editor.name')}</span><input bind:value={f.name} maxlength="64" /></label>
    <label class="field"><span>{t('apps.editor.compose')}</span>
      <textarea class="mono" rows="14" spellcheck="false" bind:value={f.compose} readonly={mig}></textarea></label>
    <label class="field"><span>{t('apps.editor.load')}</span><input type="file" accept=".yml,.yaml,.json,text/yaml" onchange={loadFile} /></label>
    {#if edit}
      <label class="row"><input type="checkbox" bind:checked={f.keepEnv} />
        {t('apps.editor.envKeep', { keys: edit.env_keys?.length ? edit.env_keys.join(', ') : t('apps.editor.envNone') })}</label>
    {/if}
    {#if !f.keepEnv}
      <label class="field"><span>{t('apps.editor.env')}</span>
        <textarea class="mono" rows="4" spellcheck="false" autocomplete="off" bind:value={f.env}></textarea>
        <span class="small muted">{t('apps.editor.envHint')}</span></label>
    {/if}
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <div class="row">
      <button disabled={busy || !validId(f.id) || !f.compose.trim()} onclick={doCheck}>{busy && !check ? t('apps.editor.checking') : t('apps.editor.check')}</button>
      {#if stale}<span class="small muted" role="status">{t('apps.editor.recheck')}</span>{/if}
    </div>

    {#if check}
      {#if check.forbidden.length}
        <div class="banner bad"><b>{t('apps.editor.forbidden')}</b>
          <ul>{#each check.forbidden as i}<li>{#if i.service}<b>{i.service}</b>: {/if}{i.detail}</li>{/each}</ul></div>
      {/if}
      {#if check.conflicts.length}
        <div class="banner bad"><b>{t('apps.editor.conflicts')}</b> <HelpButton section="apps.ports" />
          <ul>{#each check.conflicts as i}<li>{i.detail}</li>{/each}</ul></div>
      {/if}
      {#if own && Object.keys(check.run_as || {}).length}
        <div class="stack"><b>{t('apps.access.runTitle')}</b> <HelpButton section="apps.access" />
          {#each Object.keys(check.run_as).sort() as svc}
            <label class="row"><span>{svc}</span>
              <select value={runAs[svc] || check.run_as[svc]} onchange={(e) => (runAs = { ...runAs, [svc]: e.currentTarget.value })}>
                {#each ['uid', 'puid', 'root'] as m}<option value={m}>{t('apps.access.mode.' + m)}</option>{/each}
              </select></label>
          {/each}</div>
      {/if}
      {#if own && rows.length}
        <div class="stack"><b>{t('apps.access.sharesTitle')}</b>
          {#each rows as r}
            <label class="row"><span>{r.share} <span class="small muted">({r.services.join(', ')})</span></span>
              <select value={grants[r.share] || 'ro'} onchange={(e) => (grants = { ...grants, [r.share]: e.currentTarget.value })}>
                <option value="ro">{t('apps.access.ro')}</option><option value="rw">{t('apps.access.rw')}</option>
              </select></label>
          {/each}</div>
      {/if}
      {#if check.ports.length}
        <div class="stack"><b>{t('apps.editor.ports')}</b>
          {#each check.ports as p}
            <label class="row"><input type="checkbox" checked={network.has(portKey(p))}
                onchange={(e) => (network = toggle(network, portKey(p), e.currentTarget.checked))} />
              {p.host}/{p.proto} → {p.service}:{p.container} — {t('apps.editor.network')}
              <span class="small muted">{t('apps.editor.networkHint')}</span></label>
          {/each}</div>
      {/if}
      {#if check.risks.length}
        <div class="banner warn stack"><b>{t('apps.editor.risks')}</b> <HelpButton section="apps.risks" />
          {#each check.risks as r}
            <label class="row"><input type="checkbox" checked={confirm.has(r.key)}
                onchange={(e) => (confirm = toggle(confirm, r.key, e.currentTarget.checked))} />
              {#if r.service}<b>{r.service}</b>:{/if} {r.detail}</label>
          {/each}</div>
      {/if}
      {#if check.changes.length}
        <details><summary>{t('apps.editor.changes')} ({check.changes.length})</summary>
          <ul class="small">{#each check.changes as i}<li>{#if i.service}<b>{i.service}</b>: {/if}{i.detail}</li>{/each}</ul></details>
      {/if}
      {#if Object.keys(check.shares || {}).length}
        <p class="small muted">{t('apps.editor.shares', { list: Object.entries(check.shares).map(([n, p]) => `${n}: ${p}`).join('; ') })}</p>
      {/if}
      {#if check.no_health.length}<p class="small muted">{t('apps.editor.noHealth', { list: check.no_health.join(', ') })}</p>{/if}
      {#if check.normalized}
        <details><summary>{t('apps.editor.normalized')}</summary><pre class="mono small">{check.normalized}</pre></details>
      {/if}
      {#if ready(check, confirm) && !stale}<p class="small" role="status">{t('apps.editor.ok')}</p>{/if}
    {/if}
    <div class="row">
      <button class="primary" disabled={!canSave} onclick={save}>{mig ? t('apps.access.migrate') : edit ? t('apps.editor.save') : t('apps.editor.create')}</button>
      <button onclick={onclose}>{t('common.cancel')}</button>
    </div>
  </div>
</Drawer>
