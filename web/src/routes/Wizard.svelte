<script>
  import { app, flash, refresh } from '../lib/state.svelte.js';
  import { get, post, put } from '../lib/api.js';
  import { bytes } from '../lib/format.js';
  import IdentityForm from '../lib/IdentityForm.svelte';
  import PoolWizard from '../lib/PoolWizard.svelte';
  import RestoreWizard from '../lib/RestoreWizard.svelte';
  import FoundPools from '../lib/FoundPools.svelte';
  import { loadSetup } from '../lib/state.svelte.js';
  import { t } from '../lib/i18n/index.js';

  // Мастер первоначальной настройки. Шаг 1 — администратор по токену
  // установки — выполнен на странице входа. Повторного прохода нет: после
  // «Завершить» сервер отвечает 409, имя и время меняются в настройках.
  let step = $state(2);
  let poolDrawer = $state(false);
  let share = $state({ name: '', pool: '', comment: '' });
  let shareDone = $state('');
  let err = $state('');
  let busy = $state(false);
  let identity = $state(null);
  // Восстановление из копии вместо шагов 2–4.
  let restoring = $state(false);
  // Пул подхвачен — предложить восстановление из копии на нём.
  let restoreOffer = $state(false);

  const me = $derived(app.session.user.name);
  const shareOk = $derived(/^[A-Za-z0-9][A-Za-z0-9_-]{0,31}$/.test(share.name) && !/^homes$/i.test(share.name));

  $effect(() => {
    if (!share.pool && app.pools.length) share.pool = app.pools[0].name;
  });

  async function makeShare(e) {
    e.preventDefault();
    busy = true;
    err = '';
    try {
      await post('/nas/shares', { name: share.name, pool: share.pool, comment: share.comment });
      // Создатель получает полный доступ: иначе папка никому не доступна.
      await put(`/nas/shares/${encodeURIComponent(share.name)}/access`, { access: [{ kind: 'user', subject: me, level: 'rw' }] });
      shareDone = share.name;
      flash(t('wizard.folderCreated'));
      step = 5;
    } catch (x) {
      err = x.message;
    } finally {
      busy = false;
    }
  }

  async function finish() {
    busy = true;
    err = '';
    try {
      app.setup = await post('/setup/complete');
    } catch (x) {
      err = x.message;
      if (x.status === 409) app.setup = { ...app.setup, done: true };
    } finally {
      busy = false;
    }
  }
  const titles = { get 2() { return t('wizard.titles.identity'); }, get 3() { return t('wizard.titles.pool'); }, get 4() { return t('wizard.titles.share'); }, get 5() { return t('wizard.titles.done'); } };
</script>

<div class="login card wizard stack">
  <h1>{t('wizard.heading', { product: app.product })}</h1>
  <p class="muted small">{t('wizard.stepBefore', { step, title: titles[step] })}<b>{me}</b>{t('wizard.stepAfter')}</p>

  {#if restoring}
    <RestoreWizard oncancel={() => (restoring = false)} ondone={async () => { restoring = false; await refresh(); await loadSetup(); }} />
  {:else if step === 2}
    <div class="set">
      <span class="sp"><b>{t('wizard.haveBackup')}</b><small>{t('wizard.haveBackupHint')}</small></span>
      <button type="button" onclick={() => (restoring = true)}>{t('wizard.restoreFromBackup')}</button>
    </div>
    <IdentityForm submitLabel={t('wizard.saveAndContinue')} onsaved={(r) => { identity = r.identity; step = 3; }} />
  {:else if step === 3}
    <p>{t('wizard.poolIntro')}</p>
    <FoundPools onimported={() => (restoreOffer = true)} />
    {#if restoreOffer}
      <div class="set"><span class="sp"><b>{t('wizard.poolRestore')}</b><small>{t('wizard.poolRestoreHint')}</small></span>
        <button type="button" onclick={() => (restoring = true)}>{t('wizard.restoreFromBackup')}</button></div>
    {/if}
    {#each app.pools as p}
      <div class="row between"><b>{p.name}</b><span class="muted small">{t('wizard.poolDisks', { count: p.disks?.length ?? 0, size: bytes((p.disks || []).reduce((a, d) => a + d.size_bytes, 0)) })}</span></div>
    {:else}
      <p class="muted">{t('wizard.noPools')}</p>
    {/each}
    <div class="row">
      <button onclick={() => (poolDrawer = true)}>{t('common.createPool')}</button>
      <button class="primary" onclick={() => (step = app.pools.length ? 4 : 5)}>{app.pools.length ? t('wizard.next') : t('wizard.skip')}</button>
    </div>
  {:else if step === 4}
    <p>{t('wizard.shareIntro')}</p>
    <form onsubmit={makeShare} class="stack">
      <label class="field"><span>{t('wizard.folderName')}</span><input bind:value={share.name} autocomplete="off" required /></label>
      {#if share.name && !shareOk}<p class="error">{t('wizard.badName')}</p>{/if}
      <label class="field"><span>{t('common.pool')}</span>
        <select bind:value={share.pool}>{#each app.pools as p}<option value={p.name}>{p.name}</option>{/each}</select>
      </label>
      <label class="field"><span>{t('wizard.comment')}</span><input bind:value={share.comment} /></label>
      {#if err}<p class="error" role="alert">{err}</p>{/if}
      <div class="row"><button class="primary" disabled={busy || !shareOk || !share.pool}>{t('wizard.createFolder')}</button><button type="button" onclick={() => { err = ''; step = 5; }}>{t('wizard.skip')}</button></div>
    </form>
  {:else}
    <p>{t('wizard.allDone')}</p>
    <ul>
      <li>{t('wizard.summary.admin')}<b>{me}</b></li>
      {#if identity}<li>{t('wizard.summary.server')}<b>{identity.hostname}</b>{t('wizard.summary.timezone', { tz: identity.timezone })}</li>{/if}
      <li>{t('wizard.summary.pools')}{app.pools.length ? app.pools.map((p) => p.name).join(', ') : t('wizard.summary.poolsNone')}</li>
      <li>{t('wizard.summary.folder')}{shareDone || t('wizard.summary.folderNone')}</li>
    </ul>
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <button class="primary" disabled={busy} onclick={finish}>{t('wizard.finish')}</button>
  {/if}
</div>

{#if poolDrawer}<PoolWizard onclose={() => { poolDrawer = false; refresh(); }} />{/if}
