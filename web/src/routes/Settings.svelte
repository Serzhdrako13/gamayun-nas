<script>
  import HelpButton from '../help/HelpButton.svelte';
  import { onMount } from 'svelte';
  import { app, flash, setTheme, setShell, setLang } from '../lib/state.svelte.js';
  import { get, put, post, del } from '../lib/api.js';
  import IdentityForm from '../lib/IdentityForm.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import Switch from '../lib/Switch.svelte';
  import Seg from '../lib/Seg.svelte';
  import { t } from '../lib/i18n/index.js';

  let homes = $state({ enabled: false, pool: '' });
  let homesPool = $state('');
  let guard = $state(null);
  let status = $state(null);
  let g = $state({ enabled: false, maxretry: 5, findtime: 10, bantime: 10, ignore: '' });
  let err = $state('');
  let askOff = $state(false);
  // Служба SSH.
  let ssh = $state(null);
  let sshAsk = $state(false);
  let sshKill = $state(false);
  async function loadSSH() {
    try {
      ssh = await get('/system/ssh');
    } catch (e) {
      err = e.message;
    }
  }
  const setSSH = (enabled) =>
    act(async () => {
      sshAsk = false;
      ssh = await put('/system/ssh', { enabled, kill_sessions: !enabled && sshKill });
      sshKill = false;
    }, enabled ? t('settings.sshSvc.on') : t('settings.sshSvc.off'));
  // Блокировки веб-входа.
  let locks = $state(null);
  async function loadLocks() {
    try {
      locks = (await get('/auth/lockouts')).lockouts;
    } catch (e) {
      locks = [];
      err = e.message;
    }
  }
  const unlock = (id) =>
    act(async () => {
      await del('/auth/lockouts/' + id);
      await loadLocks();
    }, t('settings.locks.unlocked'));
  const unlockAll = () =>
    act(async () => {
      await del('/auth/lockouts');
      await loadLocks();
    }, t('settings.locks.allUnlocked'));
  const LOCK = {
    ip: (l) => t('settings.locks.ip', { address: l.address }),
    account: (l) => (l.unknown_user ? t('settings.locks.unknownUser') : t('settings.locks.account', { user: l.user })),
    device: (l) => t('settings.locks.device', { user: l.user }),
  };
  // Сертификат HTTPS.
  let cert = $state(null);
  let pem = $state({ cert: '', key: '' });
  let askSelf = $state(false);
  async function loadCert() {
    try {
      cert = await get('/system/tls');
    } catch (e) {
      cert = { error: e.message };
    }
  }
  const readFile = (field) => async (e) => {
    const f = e.currentTarget.files?.[0];
    if (f) pem[field] = await f.text();
  };
  const saveCert = (e) => {
    e.preventDefault();
    act(async () => {
      cert = await put('/system/tls', pem);
      pem = { cert: '', key: '' };
    }, t('settings.cert.installed'));
  };
  const selfCert = () =>
    act(async () => {
      askSelf = false;
      cert = await del('/system/tls');
    }, t('settings.cert.reverted'));
  const SRC = {
    get config() { return t('settings.cert.src.config'); },
    get custom() { return t('settings.cert.src.custom'); },
    get 'self-signed'() { return t('settings.cert.src.selfSigned'); },
  };
  // Проверки по расписанию.
  let sched = $state(null);
  let temp = $state(null); // пороги температуры

  async function loadSched() {
    try {
      const m = await get('/maintenance');
      sched = m.schedule;
      temp = m.temperature;
    } catch (e) {
      err = e.message;
    }
  }
  const saveSched = (e) => {
    e.preventDefault();
    act(async () => {
      const num = (x) => ({ enabled: x.enabled, interval_days: Number(x.interval_days) });
      sched = await put('/maintenance/schedule', {
        scrub: num(sched.scrub),
        smart_short: num(sched.smart_short),
        smart_long: num(sched.smart_long),
        hour: Number(sched.hour),
      });
    }, t('settings.sched.saved'));
  };
  const saveTemp = (e) => {
    e.preventDefault();
    act(async () => {
      const lim = (x) => ({ warning_c: Number(x.warning_c), critical_c: Number(x.critical_c) });
      temp = await put('/maintenance/temperature', { hdd: lim(temp.hdd), ssd: lim(temp.ssd) });
    }, t('settings.temp.saved'));
  };

  // Сроки хранения журнала событий и центра уведомлений.
  let ret = $state(null);
  async function loadRet() {
    try {
      ret = await get('/system/retention');
    } catch (e) {
      err = e.message;
    }
  }
  const saveRet = (e) => {
    e.preventDefault();
    act(async () => {
      const r = await put('/system/retention', {
        info_days: Number(ret.info_days),
        warning_days: Number(ret.warning_days),
        critical_days: Number(ret.critical_days),
      });
      ret = r.retention;
    }, t('settings.retention.saved'));
  };

  const MIN = 60;
  function fillGuard(r) {
    guard = r.settings;
    status = r.status;
    g = {
      enabled: guard.enabled,
      maxretry: guard.maxretry,
      findtime: Math.round(guard.findtime / MIN),
      bantime: Math.round(guard.bantime / MIN),
      ignore: (guard.ignore || []).join(' '),
    };
  }
  async function load() {
    try {
      homes = await get('/nas/homes');
      homesPool = homes.pool || app.pools[0]?.name || '';
      fillGuard(await get('/system/ssh-guard'));
    } catch (e) {
      err = e.message;
    }
  }
  onMount(() => {
    load();
    loadSched();
    loadCert();
    loadLocks();
    loadRet();
    loadSSH();
  });

  async function act(fn, ok) {
    err = '';
    try {
      await fn();
      if (ok) flash(ok);
    } catch (e) {
      err = e.message;
    }
  }
  const setHomes = (enabled) =>
    act(async () => {
      homes = await put('/nas/homes', { enabled, pool: enabled ? homesPool : homes.pool });
    }, enabled ? t('settings.homes.enabled') : t('settings.homes.disabled'));
  const saveGuard = (e) => {
    e.preventDefault();
    act(async () => {
      fillGuard(
        await put('/system/ssh-guard', {
          enabled: g.enabled,
          maxretry: Number(g.maxretry),
          findtime: Number(g.findtime) * MIN,
          bantime: Number(g.bantime) * MIN,
          ignore: g.ignore.split(/[\s,]+/).filter(Boolean),
        }),
      );
    }, t('settings.ssh.saved'));
  };
  const unban = (ip) =>
    act(async () => {
      await post('/system/ssh-guard/unban', { ip });
      fillGuard(await get('/system/ssh-guard'));
    }, t('settings.ssh.unbanned'));
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
<div class="grid">
  <section class="card stack">
    <h2>{t('settings.identity.title')} <HelpButton section="settings" /></h2>
    <IdentityForm />
    <p class="small muted">{t('settings.identity.note')}</p>
  </section>

  <section class="card stack">
    <h2>{t('settings.view.title')} <HelpButton section="settings" /></h2>
    <div class="set">
      <span class="sp"><b>{t('settings.view.style')}</b><small>{t('settings.view.styleHint')}</small></span>
      <Seg options={[['tabs', t('settings.view.tabs')], ['desktop', t('settings.view.desktop')]]} value={app.shell} label={t('settings.view.style')} onchange={setShell} />
    </div>
    <div class="set">
      <span class="sp"><b>{t('settings.view.theme')}</b><small>{t('settings.view.themeHint')}</small></span>
      <Seg options={[['dark', t('settings.view.dark')], ['light', t('settings.view.light')], ['auto', t('settings.view.auto')]]} value={app.theme} label={t('settings.view.theme')} onchange={setTheme} />
    </div>
    <div class="set">
      <span class="sp"><b>{t('settings.lang')}</b><small>{t('settings.view.langHint')}</small></span>
      <Seg options={[['ru', t('settings.langNames.ru')], ['en', t('settings.langNames.en')]]} value={app.lang} label={t('settings.lang')} onchange={setLang} />
    </div>
  </section>

  <section class="card stack">
    <h2>{t('settings.sched.title')} <HelpButton section="settings" /></h2>
    {#if sched}
      <form onsubmit={saveSched} class="stack">
        <Switch label={t('settings.sched.scrub')} hint={t('settings.sched.scrubHint')} checked={sched.scrub.enabled} onchange={(v) => (sched.scrub.enabled = v)} />
        <label class="field"><span>{t('settings.sched.everyDays')}</span><input type="number" min="1" max="365" bind:value={sched.scrub.interval_days} /></label>
        <Switch label={t('settings.sched.smartShort')} hint={t('settings.sched.smartShortHint')} checked={sched.smart_short.enabled} onchange={(v) => (sched.smart_short.enabled = v)} />
        <label class="field"><span>{t('settings.sched.everyDays')}</span><input type="number" min="1" max="365" bind:value={sched.smart_short.interval_days} /></label>
        <Switch label={t('settings.sched.smartLong')} hint={t('settings.sched.smartLongHint')} checked={sched.smart_long.enabled} onchange={(v) => (sched.smart_long.enabled = v)} />
        <label class="field"><span>{t('settings.sched.everyDays')}</span><input type="number" min="1" max="365" bind:value={sched.smart_long.interval_days} /></label>
        <label class="field"><span>{t('settings.sched.hour')}</span><input type="number" min="0" max="23" bind:value={sched.hour} /></label>
        <button class="primary">{t('common.save')}</button>
      </form>
    {:else}<p class="muted">{t('common.loading')}</p>{/if}
  </section>

  <section class="card stack">
    <h2>{t('settings.temp.title')} <HelpButton section="settings" /></h2>
    {#if temp}
      <form onsubmit={saveTemp} class="stack">
        {#each [['hdd', 'HDD'], ['ssd', t('settings.temp.ssd')]] as [k, title]}
          <div class="row">
            <label class="field"><span>{t('settings.temp.warning', { title })}</span><input type="number" min="30" max="99" bind:value={temp[k].warning_c} /></label>
            <label class="field"><span>{t('settings.temp.critical', { title })}</span><input type="number" min="31" max="100" bind:value={temp[k].critical_c} /></label>
          </div>
        {/each}
        <button class="primary">{t('common.save')}</button>
      </form>
    {:else}<p class="muted">{t('common.loading')}</p>{/if}
  </section>

  <section class="card stack">
    <h2>{t('settings.retention.title')} <HelpButton section="settings" /></h2>
    {#if ret}
      <form onsubmit={saveRet} class="stack">
        <label class="field"><span>{t('settings.retention.info')}</span><input type="number" min="1" max="3650" bind:value={ret.info_days} /></label>
        <label class="field"><span>{t('settings.retention.warning')}</span><input type="number" min="1" max="3650" bind:value={ret.warning_days} /></label>
        <label class="field"><span>{t('settings.retention.critical')}</span><input type="number" min="1" max="3650" bind:value={ret.critical_days} /></label>
        <button class="primary">{t('common.save')}</button>
      </form>
    {:else}<p class="muted">{t('common.loading')}</p>{/if}
  </section>

  <section class="card stack">
    <h2>{t('settings.homes.title')} <HelpButton section="settings" /></h2>
    {#each homes.warnings || [] as w}<p class="banner warn">{w}</p>{/each}
    {#if homes.enabled}
      <Switch label={t('settings.homes.title')} hint={t('settings.homes.onPool', { pool: homes.pool })} checked={true} onchange={() => (askOff = true)} />
      {#if askOff}
        <div class="row">
          <span>{t('settings.homes.askOff')}</span>
          <button class="danger" onclick={() => { askOff = false; setHomes(false); }}>{t('settings.homes.confirmOff')}</button>
          <button onclick={() => (askOff = false)}>{t('common.cancel')}</button>
        </div>
      {/if}
    {:else if app.pools.length}
      <label class="field"><span>{t('common.pool')}</span><select bind:value={homesPool}>{#each app.pools as p}<option value={p.name}>{p.name}</option>{/each}</select></label>
      <Switch label={t('settings.homes.title')} hint={t('settings.homes.enableHint')} checked={false} disabled={!homesPool} onchange={() => setHomes(true)} />
    {:else}<p class="muted">{t('common.createPoolFirst')}</p>{/if}
  </section>

  <section class="card stack">
    <h2>{t('settings.sshSvc.title')} <HelpButton section="settings" /></h2>
    {#if !ssh}<p class="muted">{t('common.loading')}</p>
    {:else if !ssh.status.installed}<p class="muted">{t('settings.sshSvc.notInstalled')}</p>
    {:else}
      <Switch label={t('settings.sshSvc.enabled')} checked={ssh.enabled} onchange={(v) => (v ? setSSH(true) : (sshAsk = true))} />
      {#if sshAsk}
        <div class="stack">
          <span>{t('settings.sshSvc.askOff')}</span>
          <label class="row"><input type="checkbox" bind:checked={sshKill} /> {t('settings.sshSvc.kill')}</label>
          <div class="row">
            <button class="danger" onclick={() => setSSH(false)}>{t('settings.sshSvc.confirmOff')}</button>
            <button onclick={() => { sshAsk = false; sshKill = false; }}>{t('common.cancel')}</button>
          </div>
        </div>
      {/if}
      {#if ssh.enabled && !ssh.ssh_users}<p class="banner warn">{t('settings.sshSvc.noUsers')}</p>{/if}
      {#if ssh.status.dev_access}<p class="small muted">{t('settings.sshSvc.dev')}</p>{/if}
    {/if}
  </section>

  <section class="card stack">
    <h2>{t('settings.ssh.title')} <HelpButton section="settings" /></h2>
    {#if status && !status.installed}
      <p class="banner warn">{t('settings.ssh.noFail2ban')}</p>
    {/if}
    <form onsubmit={saveGuard} class="stack">
      <Switch label={t('settings.ssh.enabled')} checked={g.enabled} onchange={(v) => (g.enabled = v)} disabled={status && !status.installed && !g.enabled} />
      <label class="field"><span>{t('settings.ssh.maxretry')}</span><input type="number" min="1" max="20" bind:value={g.maxretry} /></label>
      <label class="field"><span>{t('settings.ssh.findtime')}</span><input type="number" min="1" max="1440" bind:value={g.findtime} /></label>
      <label class="field"><span>{t('settings.ssh.bantime')}</span><input type="number" min="1" max="10080" bind:value={g.bantime} /></label>
      <label class="field"><span>{t('settings.ssh.ignore')}</span><input bind:value={g.ignore} placeholder="192.168.1.0/24" /></label>
      <button class="primary">{t('common.save')}</button>
    </form>
    {#if status}
      <p class="small">{t('settings.ssh.status', { state: status.active ? t('settings.ssh.running') : t('settings.ssh.notRunning') })}</p>
      {#each status.banned || [] as ip}
        <div class="row between"><span>{ip} <span class="badge warn">{t('settings.ssh.banned')}</span></span><button onclick={() => unban(ip)}>{t('common.unlock')}</button></div>
      {/each}
    {/if}
  </section>
  <section class="card stack">
    <h2>{t('settings.locks.title')} <HelpButton section="settings" /></h2>
    {#if !locks}<p class="muted">{t('common.loading')}</p>
    {:else if !locks.length}<p class="small">{t('settings.locks.none')}</p>
    {:else}
      {#each locks as l (l.id)}
        <div class="row between">
          <span>{LOCK[l.scope]?.(l) ?? l.scope} <span class="badge warn">{t('settings.locks.until', { time: new Date(l.until).toLocaleTimeString() })}</span>
            <small class="muted">{t('settings.locks.fails', { count: l.fails, time: new Date(l.last_failure).toLocaleTimeString() })}</small></span>
          <button onclick={() => unlock(l.id)}>{t('common.unlock')}</button>
        </div>
      {/each}
      {#if locks.length > 1}<button onclick={unlockAll}>{t('settings.locks.unlockAll')}</button>{/if}
    {/if}
    <button onclick={loadLocks}>{t('common.refresh')}</button>
  </section>

  <section class="card stack">
    <h2>{t('settings.cert.title')} <HelpButton section="settings" /></h2>
    {#if !cert}<p class="muted">{t('common.loading')}</p>
    {:else if cert.error}<p class="muted">{cert.error}</p>
    {:else}
      {#each cert.warnings as w}<p class="banner warn">{w}</p>{/each}
      <p class="small">{t('settings.cert.source')}<b>{SRC[cert.source] || cert.source}</b>{cert.self_signed ? '' : t('settings.cert.issuedBy', { issuer: cert.issuer })}.<br />
        {t('settings.cert.for', { names: [...cert.dns_names, ...cert.ips].join(', ') })}<br />
        {t('settings.cert.validUntil', { date: new Date(cert.not_after).toLocaleDateString() })}</p>
      <p class="small muted">{t('settings.cert.fingerprint')}<br /><code class="fp">{cert.fingerprint_sha256}</code></p>
      {#if !cert.editable}
        <p class="small muted">{t('settings.cert.fromConfig')}</p>
      {:else}
        <form onsubmit={saveCert} class="stack">
          <label class="field"><span>{t('settings.cert.certFile')}</span><input type="file" accept=".pem,.crt,.cer" onchange={readFile('cert')} /></label>
          <textarea rows="3" bind:value={pem.cert} placeholder="-----BEGIN CERTIFICATE-----" spellcheck="false"></textarea>
          <label class="field"><span>{t('settings.cert.keyFile')}</span><input type="file" accept=".pem,.key" onchange={readFile('key')} /></label>
          <textarea rows="3" bind:value={pem.key} placeholder="-----BEGIN PRIVATE KEY-----" spellcheck="false" autocomplete="off"></textarea>
          <button class="primary" disabled={!pem.cert || !pem.key}>{t('settings.cert.install')}</button>
        </form>
        {#if cert.source === 'custom'}
          {#if askSelf}
            <div class="row">
              <span>{t('settings.cert.askSelf')}</span>
              <button class="danger" onclick={selfCert}>{t('settings.cert.yes')}</button>
              <button onclick={() => (askSelf = false)}>{t('common.cancel')}</button>
            </div>
          {:else}<button onclick={() => (askSelf = true)}>{t('settings.cert.revertSelf')}</button>{/if}
        {/if}
      {/if}
    {/if}
  </section>
</div>

<style>
  .fp { word-break: break-all; }
  textarea { width: 100%; font-family: monospace; font-size: 0.8em; }
</style>
