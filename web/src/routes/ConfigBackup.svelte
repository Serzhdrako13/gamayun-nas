<script>
  import HelpButton from '../help/HelpButton.svelte';
  // Резервная копия настроек — скачать, копии в пулах, USB, SFTP.
  // Пароли и секреты в копию не входят. Восстановление на настроенной системе —
  // с консоли (gamayund config restore), из веба — в мастере первого запуска.
  import { onMount } from 'svelte';
  import { flash } from '../lib/state.svelte.js';
  import { get, post, put } from '../lib/api.js';
  import { bytes } from '../lib/format.js';
  import Switch from '../lib/Switch.svelte';
  import { t } from '../lib/i18n/index.js';

  let pools = $state([]);
  let usb = $state(null);
  let sftp = $state(null);
  let cfg = $state({ enabled: false, host: '', port: 22, user: '', dir: '', host_key: '' });
  let scanned = $state(null); // {host_key, fingerprint} — ждёт подтверждения
  let err = $state('');
  let busy = $state('');

  async function act(name, fn, ok) {
    busy = name;
    err = '';
    try {
      await fn();
      if (ok) flash(ok);
    } catch (e) {
      err = e.message;
    } finally {
      busy = '';
    }
  }
  async function loadList() {
    const r = await get('/config/backups');
    pools = r.pools;
  }
  async function loadSFTP() {
    sftp = await get('/config/sftp');
    cfg = { ...sftp.config, port: sftp.config.port || 22 };
    scanned = null;
  }
  onMount(() => act('load', async () => { await loadList(); await loadSFTP(); }));

  const saveNow = () => act('save', async () => {
    const r = await post('/config/backup');
    const bad = r.pools.filter((p) => p.error);
    await loadList();
    if (bad.length) throw new Error(bad.map((p) => `${p.pool || 'SFTP'}: ${p.error}`).join('; '));
  }, t('confbackup.saved'));
  const findUSB = () => act('usb', async () => { usb = (await get('/config/usb')).devices; });
  const toUSB = (d) => act('usb', async () => {
    const r = await post('/config/usb', { device: d.device });
    flash(t('confbackup.usbWritten', { path: r.path }));
  });
  const scan = () => act('scan', async () => {
    scanned = await post('/config/sftp/scan', { host: cfg.host, port: Number(cfg.port) });
  });
  const acceptKey = () => {
    cfg.host_key = scanned.host_key;
    scanned = null;
  };
  const saveSFTP = (e) => {
    e.preventDefault();
    act('sftp', async () => {
      sftp = await put('/config/sftp', { ...cfg, port: Number(cfg.port) });
      cfg = { ...sftp.config };
    }, t('confbackup.sftp.saved'));
  };
  const uploadNow = () => act('upload', async () => {
    const r = await post('/config/sftp/upload');
    flash(t('confbackup.sftp.uploaded', { path: r.path }));
  });
  const fp = $derived(sftp && cfg.host_key === sftp.config.host_key ? sftp.host_fingerprint : cfg.host_key ? t('confbackup.sftp.newKey') : '');
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
<div class="grid">
  <section class="card stack">
    <h2>{t('confbackup.title')} <HelpButton section="settings.confbackup" /></h2>
    <div class="row">
      <a class="btn primary" href="/api/v1/config/backup/download" download>{t('confbackup.download')}</a>
      <button disabled={!!busy} onclick={saveNow}>{t('confbackup.saveNow')}</button>
    </div>
  </section>

  <section class="card stack">
    <h2>{t('confbackup.inPools')}</h2>
    {#each pools as p}
      <h3>{p.pool}</h3>
      {#if p.error}<p class="error small">{p.error}</p>{/if}
      {#each p.files.slice(0, 10) as f}
        <div class="row between"><a href={`/api/v1/config/backups/${encodeURIComponent(p.pool)}/${encodeURIComponent(f.name)}`} download>{f.name}</a><span class="muted small">{bytes(f.size)}</span></div>
      {:else}
        {#if !p.error}<p class="muted small">{t('confbackup.noCopies')}</p>{/if}
      {/each}
      {#if p.files.length > 10}<p class="muted small">{t('confbackup.andMore', { count: p.files.length - 10 })}</p>{/if}
    {:else}
      <p class="muted">{t('confbackup.noPools')}</p>
    {/each}
  </section>

  <section class="card stack">
    <h2>{t('confbackup.usb.title')}</h2>
    <p class="small muted">{t('confbackup.usb.hint')}<code>gamayun-nas-config</code>.</p>
    <button disabled={!!busy} onclick={findUSB}>{t('confbackup.usb.find')}</button>
    {#if usb}
      {#each usb as d}
        <div class="row between"><span>{d.model || d.device} · {d.label || t('confbackup.usb.noLabel')} · {d.fstype} · {bytes(d.size)}</span><button disabled={!!busy} onclick={() => toUSB(d)}>{t('confbackup.usb.write')}</button></div>
      {:else}
        <p class="muted small">{t('confbackup.usb.none')}</p>
      {/each}
    {/if}
  </section>

  <section class="card stack">
    <h2>{t('confbackup.sftp.title')}</h2>
    {#if sftp}
      <p class="small muted">{t('confbackup.sftp.keyBefore')}<code>~/.ssh/authorized_keys</code>{t('confbackup.sftp.keyAfter')}</p>
      <textarea readonly rows="3" onfocus={(e) => e.currentTarget.select()}>{sftp.public_key}</textarea>
      <p class="small muted">{t('confbackup.sftp.nasFingerprint')}<code>{sftp.key_fingerprint}</code></p>
      <form class="stack" onsubmit={saveSFTP}>
        <Switch label={t('confbackup.sftp.auto')} hint={t('confbackup.sftp.autoHint')} checked={cfg.enabled} onchange={(v) => (cfg.enabled = v)} />
        <label class="field"><span>{t('common.server')}</span><input bind:value={cfg.host} oninput={() => (cfg.host_key = '')} autocomplete="off" /></label>
        <label class="field"><span>{t('common.port')}</span><input type="number" min="1" max="65535" bind:value={cfg.port} oninput={() => (cfg.host_key = '')} /></label>
        <label class="field"><span>{t('confbackup.sftp.user')}</span><input bind:value={cfg.user} autocomplete="off" /></label>
        <label class="field"><span>{t('confbackup.sftp.dir')}</span><input bind:value={cfg.dir} autocomplete="off" /></label>
        <div class="set">
          <span class="sp"><b>{t('confbackup.sftp.hostKey')}</b><small>{fp || t('confbackup.sftp.notConfirmed')}</small></span>
          <button type="button" disabled={!!busy || !cfg.host} onclick={scan}>{t('confbackup.sftp.checkFingerprint')}</button>
        </div>
        {#if scanned}
          <div class="card stack">
            <p>{t('confbackup.sftp.presented', { host: cfg.host })}</p>
            <p><code>{scanned.fingerprint}</code></p>
            <p class="small muted">{t('confbackup.sftp.compareBefore')}<code>ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub</code>{t('confbackup.sftp.compareAfter')}</p>
            <div class="row"><button type="button" class="primary" onclick={acceptKey}>{t('confbackup.sftp.matches')}</button><button type="button" onclick={() => (scanned = null)}>{t('common.cancel')}</button></div>
          </div>
        {/if}
        <div class="row">
          <button class="primary" disabled={!!busy || !cfg.host_key}>{t('common.save')}</button>
          <button type="button" disabled={!!busy || !sftp.config.host_key} onclick={uploadNow}>{t('confbackup.sftp.uploadNow')}</button>
        </div>
      </form>
    {/if}
  </section>

  <section class="card stack">
    <h2>{t('confbackup.restore.title')}</h2>
    <p class="small muted">{t('confbackup.restore.console')}</p>
    <pre class="small">{t('confbackup.restore.commands')}</pre>
    <p class="small muted">{t('confbackup.restore.newInstall')}</p>
  </section>
</div>
