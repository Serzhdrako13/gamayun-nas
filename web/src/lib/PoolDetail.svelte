<script>
  import { onMount } from 'svelte';
  import Drawer from './Drawer.svelte';
  import PoolBadge from './PoolBadge.svelte';
  import { app, refresh, flash } from './state.svelte.js';
  import { get, post, put, del } from './api.js';
  import { bytes, POOL_TYPES, protectionBanner, dateTime, syncText } from './format.js';
  import { t } from './i18n/index.js';
  import HelpButton from '../help/HelpButton.svelte';
  import { poolTopic } from '../help/topic.js';
  import { hasHelp } from '../help/content.js';

  let { name, onclose } = $props();
  const pool = $derived(app.pools.find((p) => p.name === name));
  const banner = $derived(pool ? protectionBanner(pool) : null);
  // «Что происходит» — строки status.issues и раздел справки по полям статуса.
  const issues = $derived(pool?.status?.issues || []);
  // Раздел ещё не написан — без «?» (HelpButton скрыт), блок только при строках issues.
  const topic = $derived(pool && hasHelp(poolTopic(pool)) ? poolTopic(pool) : null);

  let reason = $state('');
  let minutes = $state(60);
  let confirmName = $state('');
  let err = $state('');
  let busy = $state(false);
  let log = $state([]);
  let lastScrub = $state(null);
  // Метка занятости пула от gamayund (pool.busy, internal/storage/home/service.go TaskScrub) —
  // протокол, не текст интерфейса; кодом станет позже.
  const SCRUB = '\u043f\u0440\u043e\u0432\u0435\u0440\u043a\u0430 \u0434\u0430\u043d\u043d\u044b\u0445'; // «проверка данных»

  async function loadScrub() {
    try {
      lastScrub = (await get('/maintenance')).last?.['scrub:' + name] || null;
    } catch {
      lastScrub = null;
    }
  }

  async function loadLog() {
    try {
      log = (await get(`/storage/pools/${encodeURIComponent(name)}/protection/log?limit=10`)).log || [];
    } catch {
      log = [];
    }
  }
  onMount(() => {
    loadLog();
    loadScrub();
  });

  async function run(fn, ok) {
    busy = true;
    err = '';
    try {
      await fn();
      if (ok) flash(ok);
      await refresh();
      await loadLog();
    } catch (e) {
      err = e.message;
    } finally {
      busy = false;
    }
  }

  const allow = () =>
    run(
      () => post(`/storage/pools/${encodeURIComponent(name)}/write-override`, { reason, duration_minutes: Number(minutes) }),
      t('poolDetail.flash.allowed'),
    );
  const revoke = () => run(() => del(`/storage/pools/${encodeURIComponent(name)}/write-override`, { reason: t('poolDetail.revokeReason') }), t('poolDetail.flash.revoked'));
  // Выключатель защиты — подтверждение с риском и обязательной причиной.
  let protWant = $state(null); // null — без изменений; true/false — что подтверждаем
  let protReason = $state('');
  const protOn = $derived(pool?.protection?.enabled !== false);
  const setProt = (enabled) =>
    run(async () => {
      await put(`/storage/pools/${encodeURIComponent(name)}/protection`, { enabled, reason: protReason.trim() });
      protWant = null;
      protReason = '';
    }, t(enabled ? 'poolDetail.flash.protectOn' : 'poolDetail.flash.protectOff'));
  // «Продолжить замену»: прерванная операция с диском без перезапуска gamayund.
  const resume = () => run(() => post(`/storage/pools/${encodeURIComponent(name)}/resume`), t('poolDetail.flash.resumed'));
  // Проверка пула (md check + btrfs scrub), итог — в уведомлениях.
  const scrub = () => run(() => post(`/storage/pools/${encodeURIComponent(name)}/scrub`), t('poolDetail.flash.scrubStarted'));
  const scrubStop = () => run(() => del(`/storage/pools/${encodeURIComponent(name)}/scrub`), t('poolDetail.flash.scrubStopped'));
  const remove = () =>
    run(async () => {
      await del(`/storage/pools/${encodeURIComponent(name)}`, { confirm_name: confirmName });
      onclose();
    }, t('poolDetail.flash.deleteStarted'));
</script>

<Drawer title={t('poolDetail.title', { name })} {onclose}>
  {#if !pool}
    <p class="muted">{t('poolDetail.notFound')}</p>
  {:else}
    <div class="stack">
      <div class="row">
        <PoolBadge {pool} />
        <span>{POOL_TYPES[pool.type] || pool.type}{pool.integrity ? t('poolDetail.withIntegrity') : ''}</span>
        <HelpButton section="storage.pools.raid" />
      </div>
      {#if issues.length || topic}
        <section class="card pool-what">
          <h2>{t('help.what')} {#if topic}<HelpButton section={topic} />{/if}</h2>
          {#if issues.length}<ul class="small">{#each issues as s}<li>{s}</li>{/each}</ul>{/if}
        </section>
      {/if}
      {#if banner}
        <div class="banner {banner.kind}" role="alert">{banner.text}</div>
      {/if}
      <div class="small muted">{t('poolDetail.summary', { created: dateTime(pool.created_at), n: pool.disks?.length ?? 0, size: bytes((pool.disks || []).reduce((a, d) => a + d.size_bytes, 0)) })}</div>
      {#if syncText(pool)}
        <div class="banner warn" role="status">{t('poolDetail.sync.banner', { progress: syncText(pool) })} {pool.status.sync?.no_redundancy ? t('poolDetail.sync.noRedundancy') : t('poolDetail.sync.fullAfter')}</div>
      {/if}
      {#if pool.status_error}<p class="error">{t('poolDetail.statusError', { err: pool.status_error })}</p>{/if}
      {#if pool.status?.arrays?.length}
        <table>
          <thead><tr><th>{t('poolDetail.arrays.array')}</th><th>{t('poolDetail.arrays.level')}</th><th>{t('common.state')}</th></tr></thead>
          <tbody>
            {#each pool.status.arrays as a}
              <tr><td class="mono">{a.name || a.device}</td><td>{a.level}</td><td>{(a.state || []).join(', ')}</td></tr>
            {/each}
          </tbody>
        </table>
      {/if}

      {#if (pool.pending || pool.grow_pending) && !pool.busy}
        <section class="card">
          <h2>{t('poolDetail.pending.title')}</h2>
          <p class="small muted">{t('poolDetail.pending.textBefore')}{pool.pending?.disk?.id ? ` (${pool.pending.disk.id})` : ''}{t('poolDetail.pending.textAfter')}</p>
          <button class="primary" onclick={resume} disabled={busy}>{t('poolDetail.pending.resume')}</button>
        </section>
      {/if}

      <section class="card">
        <h2>{t('poolDetail.scrub.title')} <HelpButton section="storage.pools.scrub" /></h2>
        {#if lastScrub}<p class="small">{t('poolDetail.scrub.last', { time: dateTime(lastScrub) })}</p>{/if}
        {#if pool.busy === SCRUB}
          <p>{t('poolDetail.scrub.running')}</p>
          <button onclick={scrubStop} disabled={busy}>{t('poolDetail.scrub.stop')}</button>
        {:else}
          <button class="primary" onclick={scrub} disabled={busy || !!pool.busy || pool.status?.health !== 'ok'}>{t('poolDetail.scrub.start')}</button>
          {#if pool.status?.initial_sync}<p class="small muted">{t('poolDetail.scrub.afterSync')}</p>{:else if pool.status?.health && pool.status.health !== 'ok'}<p class="small muted">{t('poolDetail.scrub.onlyHealthy')}</p>{/if}
        {/if}
      </section>

      <section class="card">
        <h2>{t('poolDetail.write.title')} <HelpButton section="storage.pools.readonly" /></h2>
        {#if pool.protection?.override}
          <p>{t('poolDetail.write.allowedBy', { actor: pool.protection.override.actor, reason: pool.protection.override.reason, time: dateTime(pool.protection.override.expires_at) })}</p>
          <button onclick={revoke} disabled={busy}>{t('poolDetail.write.revoke')}</button>
        {:else if pool.status?.emergency_read_only && !pool.protection?.write_blocked}
          <p class="small muted">{t('poolDetail.write.emergencyHint')}</p>
        {:else if pool.protection?.write_blocked || pool.protection?.read_only}
          <p class="small muted">{t('poolDetail.write.readOnlyHint')}</p>
          <label class="field"><span>{t('poolDetail.write.reason')}</span><input bind:value={reason} maxlength="200" /></label>
          <label class="field">
            <span>{t('poolDetail.write.term')}</span>
            <select bind:value={minutes}>
              <option value={15}>{t('poolDetail.write.min15')}</option>
              <option value={60}>{t('poolDetail.write.h1')}</option>
              <option value={360}>{t('poolDetail.write.h6')}</option>
              <option value={1440}>{t('poolDetail.write.h24')}</option>
              <option value={10080}>{t('poolDetail.write.d7')}</option>
            </select>
          </label>
          <button class="primary" onclick={allow} disabled={busy || !reason.trim()}>{t('poolDetail.write.allow')}</button>
        {:else}
          <p class="small muted">{protOn ? t('poolDetail.write.writable') : t('poolDetail.protect.offHint')}</p>
        {/if}
      </section>

      <section class="card">
        <h2>{t('poolDetail.protect.title')} <HelpButton section="storage.pools.readonly" /></h2>
        <div class="stack" role="radiogroup" aria-label={t('poolDetail.protect.title')}>
          <label><input type="radio" name="prot-{name}" checked={protWant === null ? protOn : protWant} onchange={() => (protWant = protOn ? true : null)} disabled={busy} /> {t('poolDetail.protect.on')}</label>
          <label><input type="radio" name="prot-{name}" checked={protWant === null ? !protOn : !protWant} onchange={() => (protWant = protOn ? false : null)} disabled={busy} /> {t('poolDetail.protect.off')}</label>
        </div>
        <p class="small muted">{protOn ? t('poolDetail.protect.onHint') : t('poolDetail.protect.offHint')}</p>
        {#if pool.status?.emergency_read_only}<p class="small">{t('poolDetail.protect.emergency')}</p>{/if}
        {#if protWant !== null}
          {#if protWant === false}
            <div class="banner bad" role="alert">{t('poolDetail.protect.risk')}</div>
            {#if pool.protection?.enforced}<p class="small">{t('poolDetail.protect.roNow')}</p>{/if}
          {:else}
            <p class="small">{t('poolDetail.protect.enableNow')}</p>
          {/if}
          <label class="field"><span>{t('poolDetail.protect.reason')}</span><input bind:value={protReason} maxlength="200" /></label>
          <div class="row">
            <button class={protWant ? 'primary' : 'danger'} onclick={() => setProt(protWant)} disabled={busy || !protReason.trim()}>{protWant ? t('poolDetail.protect.confirmOn') : t('poolDetail.protect.confirmOff')}</button>
            <button onclick={() => { protWant = null; protReason = ''; }} disabled={busy}>{t('poolDetail.protect.cancel')}</button>
          </div>
        {/if}
      </section>

      {#if log.length}
        <section>
          <h2>{t('poolDetail.log')}</h2>
          <ul class="small">
            {#each log as l}<li>{dateTime(l.at)} — {l.actor}: {l.action}{l.reason ? t('poolDetail.logReason', { reason: l.reason }) : ''}</li>{/each}
          </ul>
        </section>
      {/if}

      <section class="card">
        <h2>{t('poolDetail.delete.title')}</h2>
        <p class="small muted">{t('poolDetail.delete.hint')}</p>
        <label class="field"><span>{t('poolDetail.delete.nameLabel')}<b>{name}</b></span><input bind:value={confirmName} autocomplete="off" /></label>
        <button class="danger" onclick={remove} disabled={busy || confirmName !== name}>{t('poolDetail.delete.button', { name })}</button>
      </section>
      {#if err}<p class="error" role="alert">{err}</p>{/if}
    </div>
  {/if}
</Drawer>
