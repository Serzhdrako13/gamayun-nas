<script>
  import HelpButton from '../help/HelpButton.svelte';
  // Каналы внешней доставки — SMTP и webhook. Пароль и ключ подписи
  // сервер не возвращает (только «задан»); пустое поле — оставить прежний.
  import { onMount } from 'svelte';
  import { flash } from '../lib/state.svelte.js';
  import { get, put, post } from '../lib/api.js';
  import Switch from '../lib/Switch.svelte';
  import { t, locale } from '../lib/i18n/index.js';

  let smtp = $state(null);
  let hook = $state(null);
  let status = $state([]);
  let smtpPw = $state('');
  let hookSecret = $state('');
  let to = $state('');
  let err = $state('');
  let test = $state({}); // канал → {busy, ok, error}

  const PORTS = { starttls: 587, tls: 465, none: 25 };
  async function load() {
    err = '';
    try {
      const r = await get('/notifications/channels');
      smtp = { enabled: false, host: '', port: 587, security: 'starttls', username: '', from: '', to: [], ca_cert: '', insecure_skip_verify: false, ...r.smtp };
      hook = { enabled: false, url: '', ca_cert: '', insecure_skip_verify: false, allow_http: false, ...r.webhook };
      to = (smtp.to || []).join(', ');
      status = r.status || [];
    } catch (e) {
      err = e.message;
    }
  }
  onMount(load);

  function smtpBody() {
    const b = { ...smtp, to: to.split(/[,;\n]+/).map((s) => s.trim()).filter(Boolean), port: Number(smtp.port) };
    delete b.password_set;
    delete b.password;
    if (smtpPw) b.password = smtpPw;
    return b;
  }
  function hookBody() {
    const b = { ...hook };
    delete b.secret_set;
    delete b.secret;
    if (hookSecret) b.secret = hookSecret;
    return b;
  }
  async function save(ch) {
    err = '';
    try {
      if (ch === 'smtp') await put('/notifications/channels/smtp', smtpBody());
      else await put('/notifications/channels/webhook', hookBody());
      smtpPw = hookSecret = '';
      await load();
      flash(t('channels.saved'));
    } catch (e) {
      err = e.message;
    }
  }
  async function clearSecret(ch) {
    err = '';
    try {
      if (ch === 'smtp') await put('/notifications/channels/smtp', { ...smtpBody(), password: '' });
      else await put('/notifications/channels/webhook', { ...hookBody(), secret: '' });
      await load();
      flash(ch === 'smtp' ? t('channels.passwordDeleted') : t('channels.secretDeleted'));
    } catch (e) {
      err = e.message;
    }
  }
  async function check(ch) {
    test = { ...test, [ch]: { busy: true } };
    try {
      const r = await post(`/notifications/channels/${ch}/test`, ch === 'smtp' ? smtpBody() : hookBody());
      test = { ...test, [ch]: r };
    } catch (e) {
      test = { ...test, [ch]: { ok: false, error: e.message } };
    }
  }
  const st = (ch) => status.find((s) => s.channel === ch);
  const time = (x) => (x ? new Date(x).toLocaleString(locale()) : '—');
</script>

{#snippet chStatus(ch)}
  {@const s = st(ch)}
  {#if s}
    <p class="small">
      {t('channels.lastDelivery', { time: time(s.last_success), pending: s.pending })}{#if s.failed}{t('channels.failed', { count: s.failed })}{/if}{#if s.dropped}{t('channels.dropped', { count: s.dropped })}{/if}.
      {#if s.last_error}<br /><span class="badge warn">{t('channels.lastError')}</span> {s.last_error}{/if}
    </p>
  {/if}
{/snippet}

{#snippet testResult(ch)}
  {#if test[ch]?.busy}<p class="small muted">{t('channels.testing')}</p>
  {:else if test[ch]?.ok}<p class="banner ok">{t('channels.testOk')}</p>
  {:else if test[ch]}<p class="banner warn">{t('channels.testFailed', { error: test[ch].error })}</p>{/if}
{/snippet}

{#if err}<p class="error" role="alert">{err}</p>{/if}
{#if smtp && hook}
<div class="grid">
  <section class="card stack">
    <h2>{t('channels.smtp.title')} <HelpButton section="notify.channels" /></h2>
    <Switch label={t('channels.smtp.enable')} hint={t('channels.applyHint')} checked={smtp.enabled} onchange={(v) => (smtp.enabled = v)} />
    <label class="field"><span>{t('common.server')}</span><input bind:value={smtp.host} placeholder="smtp.example.org" /></label>
    <label class="field"><span>{t('channels.smtp.security')}</span>
      <select bind:value={smtp.security} onchange={() => (smtp.port = PORTS[smtp.security])}>
        <option value="starttls">{t('channels.smtp.starttls')}</option>
        <option value="tls">{t('channels.smtp.tls')}</option>
        <option value="none">{t('channels.smtp.none')}</option>
      </select>
    </label>
    <label class="field"><span>{t('common.port')}</span><input type="number" min="1" max="65535" bind:value={smtp.port} /></label>
    <label class="field"><span>{t('channels.smtp.login')}</span><input bind:value={smtp.username} autocomplete="off" /></label>
    <label class="field"><span>{t('common.password')} {#if smtp.password_set}{t('channels.setKeepEmpty')}{/if}</span><input type="password" bind:value={smtpPw} autocomplete="new-password" /></label>
    {#if smtp.password_set}<button onclick={() => clearSecret('smtp')}>{t('channels.smtp.deletePassword')}</button>{/if}
    <label class="field"><span>{t('channels.smtp.from')}</span><input bind:value={smtp.from} placeholder="NAS &lt;nas@example.org&gt;" /></label>
    <label class="field"><span>{t('channels.smtp.to')}</span><input bind:value={to} placeholder="admin@example.org" /></label>
    <details>
      <summary>{t('channels.smtp.certTitle')}</summary>
      <label class="field"><span>{t('channels.smtp.ca')}</span><textarea rows="4" bind:value={smtp.ca_cert}></textarea></label>
      <Switch label={t('channels.insecure')} hint={t('channels.smtp.insecureHint')} checked={smtp.insecure_skip_verify} onchange={(v) => (smtp.insecure_skip_verify = v)} />
    </details>
    <div class="row">
      <button class="primary" onclick={() => save('smtp')}>{t('common.save')}</button>
      <button onclick={() => check('smtp')} disabled={test.smtp?.busy}>{t('channels.smtp.test')}</button>
    </div>
    {@render testResult('smtp')}
    {@render chStatus('smtp')}
  </section>

  <section class="card stack">
    <h2>Webhook <HelpButton section="notify.channels" /></h2>
    <Switch label={t('channels.hook.enable')} hint={t('channels.applyHint')} checked={hook.enabled} onchange={(v) => (hook.enabled = v)} />
    <label class="field"><span>{t('channels.hook.url')}</span><input bind:value={hook.url} placeholder="https://monitoring.example.org/hook" /></label>
    <label class="field"><span>{t('channels.hook.secret')} {#if hook.secret_set}{t('channels.setKeepEmpty')}{/if}</span><input type="password" bind:value={hookSecret} autocomplete="new-password" /></label>
    {#if hook.secret_set}<button onclick={() => clearSecret('webhook')}>{t('channels.hook.deleteSecret')}</button>{/if}
    <details>
      <summary>{t('channels.hook.certTitle')}</summary>
      <label class="field"><span>{t('channels.hook.ca')}</span><textarea rows="4" bind:value={hook.ca_cert}></textarea></label>
      <Switch label={t('channels.insecure')} hint={t('channels.hook.insecureHint')} checked={hook.insecure_skip_verify} onchange={(v) => (hook.insecure_skip_verify = v)} />
      <Switch label={t('channels.hook.allowHttp')} hint={t('channels.hook.allowHttpHint')} checked={hook.allow_http} onchange={(v) => (hook.allow_http = v)} />
    </details>
    <div class="row">
      <button class="primary" onclick={() => save('webhook')}>{t('common.save')}</button>
      <button onclick={() => check('webhook')} disabled={test.webhook?.busy}>{t('channels.hook.test')}</button>
    </div>
    {@render testResult('webhook')}
    {@render chStatus('webhook')}
  </section>
</div>
{/if}
