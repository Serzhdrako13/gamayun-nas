<script>
  // Мастер восстановления настроек из копии. Пароли и секреты в копию
  // не входят: по умолчанию задаются новые; «оставить прежний» — только если
  // он есть на этой системе, с явным согласием (небезопасно).
  import { onMount } from 'svelte';
  import { get, post } from './api.js';
  import Seg from './Seg.svelte';
  import { initChoices, buildAnswers, anyKeep, secretsLeftEmpty, toBase64, ACTIONS } from './confbackup.js';
  import { t } from './i18n/index.js';

  let { ondone = () => {}, oncancel = () => {} } = $props();

  let src = $state(null); // {file: base64}
  let plan = $state(null);
  let ch = $state(null);
  let err = $state('');
  let busy = $state(false);
  let result = $state(null);
  // Копии настроек на пулах этой системы (в т. ч. подхваченного пула).
  let stored = $state([]);
  let pick2 = $state('');
  onMount(async () => {
    try {
      const r = await get('/config/backups');
      stored = (r.pools || []).flatMap((p) => (p.files || []).map((f) => ({ pool: p.pool, name: f.name })));
      if (stored.length) pick2 = stored[0].pool + '/' + stored[0].name;
    } catch {
      stored = [];
    }
  });
  async function useStored() {
    const [pool, ...rest] = pick2.split('/');
    src = { pool, name: rest.join('/') };
    await makePlan();
  }
  const checked = $derived(plan && ch ? buildAnswers(plan, ch) : { errors: [] });
  const empty = $derived(plan && ch ? secretsLeftEmpty(plan, ch) : []);

  async function makePlan() {
    busy = true;
    err = '';
    try {
      plan = await post('/config/restore/plan', src);
      ch = initChoices(plan);
    } catch (e) {
      err = e.message;
      plan = null;
    } finally {
      busy = false;
    }
  }
  async function pick(e) {
    const f = e.currentTarget.files?.[0];
    if (!f) return;
    if (f.size > 32 * 1024) {
      err = t('restore.fileTooBig');
      return;
    }
    src = { file: toBase64(await f.arrayBuffer()) };
    await makePlan();
  }

  async function apply(e) {
    e.preventDefault();
    if (checked.errors.length) return;
    busy = true;
    err = '';
    try {
      result = await post('/config/restore', { ...src, answers: checked.answers });
      if (!result.error) ondone(result);
    } catch (x) {
      err = x.message;
    } finally {
      busy = false;
    }
  }
</script>

<div class="stack">
  {#if result}
    <h2>{result.error ? t('restore.aborted') : t('restore.restored')}</h2>
    <ul class="small">
      {#each result.steps as s}<li>{s.what} — {s.ok ? t('restore.stepDone') : t('restore.stepError', { error: s.error })}</li>{/each}
    </ul>
    {#each result.warnings as w}<p class="small muted">⚠ {w}</p>{/each}
    {#if result.error}
      <p class="error" role="alert">{result.error}</p>
      <p class="small muted">{t('restore.retryHint')}</p>
      <div class="row"><button onclick={() => (result = null)}>{t('restore.backToAnswers')}</button><button onclick={oncancel}>{t('common.close')}</button></div>
    {:else}
      <button class="primary" onclick={() => ondone(result)}>{t('common.done')}</button>
    {/if}
  {:else if !plan}
    <p>{t('restore.pickBefore')}<code>gamayun-config-….json.gz</code>{t('restore.pickAfter')}</p>
    {#if stored.length}
      <label class="field"><span>{t('restore.onPool')}</span>
        <select bind:value={pick2}>{#each stored as f}<option value={f.pool + '/' + f.name}>{f.pool}: {f.name}</option>{/each}</select>
      </label>
      <div class="row"><button class="primary" type="button" disabled={busy || !pick2} onclick={useStored}>{t('restore.useStored')}</button></div>
    {/if}
    <label class="field"><span>{t('restore.file')}</span><input type="file" accept=".gz,application/gzip" onchange={pick} disabled={busy} /></label>
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <div class="row"><button type="button" onclick={oncancel}>{t('common.cancel')}</button></div>
  {:else}
    <p class="small muted">{t('restore.planInfo', { created: plan.created_at, version: plan.app_version })}{plan.hostname ? t('restore.planServer', { host: plan.hostname }) : ''}.</p>
    {#each plan.warnings as w}<p class="small muted">⚠ {w}</p>{/each}
    <form class="stack" onsubmit={apply}>
      <h3>{t('restore.passwords')}</h3>
      <p class="small muted">{t('restore.passwordsHint')}</p>
      {#each plan.users as u (u.name)}
        <fieldset class="stack">
          <legend><b>{u.name}</b> {u.admin ? t('restore.adminMark') : ''} {u.exists ? t('restore.exists') : ''}</legend>
          {#if u.can_keep}
            <Seg label={t('restore.passwordOf', { name: u.name })} options={[['new', t('restore.setNew')], ['keep', t('restore.keepOld')]]} value={ch.users[u.name].mode} onchange={(v) => (ch.users[u.name].mode = v)} />
          {/if}
          {#if ch.users[u.name].mode === 'new'}
            <label class="field"><span>{t('common.newPassword')}</span><input type="password" autocomplete="new-password" bind:value={ch.users[u.name].pw} /></label>
            <label class="field"><span>{t('restore.repeat')}</span><input type="password" autocomplete="new-password" bind:value={ch.users[u.name].pw2} /></label>
          {/if}
        </fieldset>
      {/each}

      {#if plan.secrets.length}
        <h3>{t('restore.secrets')}</h3>
        <p class="small muted">{t('restore.secretsHint')}</p>
        {#each plan.secrets as s (s.key)}
          <fieldset class="stack">
            <legend>{s.title}</legend>
            {#if s.can_keep}
              <Seg label={s.title} options={[['new', t('restore.setNew')], ['keep', t('restore.keepOld')]]} value={ch.secrets[s.key].mode} onchange={(v) => (ch.secrets[s.key].mode = v)} />
            {/if}
            {#if ch.secrets[s.key].mode === 'new'}
              <label class="field"><span>{t('restore.newValue')}</span><input type={s.key === 'webhook_url' ? 'url' : 'password'} autocomplete="off" bind:value={ch.secrets[s.key].value} /></label>
            {/if}
          </fieldset>
        {/each}
      {/if}

      <h3>{t('restore.whatTitle')}</h3>
      <ul class="small">
        {#each plan.groups as g}<li>{t('restore.group', { name: g.name, action: ACTIONS[g.action] })}</li>{/each}
        {#each plan.shares as s}<li>{t('restore.share', { name: s.name, action: ACTIONS[s.action] })}{s.reason ? ` (${s.reason})` : ''}</li>{/each}
        {#each plan.settings as s}<li>{s}</li>{/each}
      </ul>

      {#if anyKeep(ch)}
        <label class="set warn">
          <span class="sp"><b>{t('restore.keepTitle')}</b><small>{t('restore.keepHint')}</small></span>
          <input type="checkbox" bind:checked={ch.acceptInsecure} />
        </label>
      {/if}
      {#if empty.length}<p class="small muted">{t('restore.willDisable', { list: empty.join(', ') })}</p>{/if}
      {#each checked.errors as e}<p class="error small">{e}</p>{/each}
      {#if err}<p class="error" role="alert">{err}</p>{/if}
      <div class="row">
        <button class="primary" disabled={busy || checked.errors.length > 0}>{t('restore.restore')}</button>
        <button type="button" onclick={oncancel}>{t('common.cancel')}</button>
      </div>
    </form>
  {/if}
</div>
