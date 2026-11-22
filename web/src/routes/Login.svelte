<script>
  import { app, setSession, loadSession } from '../lib/state.svelte.js';
  import { post } from '../lib/api.js';
  import { t } from '../lib/i18n/index.js';

  let username = $state('');
  let password = $state('');
  let token = $state('');
  let err = $state('');
  let busy = $state(false);

  async function submit(e) {
    e.preventDefault();
    busy = true;
    err = '';
    try {
      if (app.setupRequired) {
        await post('/auth/setup', { setup_token: token, username, password });
      }
      await setSession(await post('/auth/login', { username, password }));
    } catch (x) {
      err = x.message;
      if (x.code === 'setup_required') app.setupRequired = true;
    } finally {
      busy = false;
      password = '';
    }
  }
</script>

<div class="login card">
  <h1>{app.product}</h1>
  {#if app.setupRequired}
    <p class="muted"><b>{t('login.setupTitle')}</b> {t('login.setupHint')}</p>
  {/if}
  <form onsubmit={submit}>
    {#if app.setupRequired}
      <label class="field"><span>{t('login.setupToken')}</span><input bind:value={token} autocomplete="off" required /></label>
    {/if}
    <label class="field"><span>{t('login.username')}</span><input bind:value={username} autocomplete="username" required /></label>
    <label class="field"><span>{t('common.password')}</span><input type="password" bind:value={password} autocomplete={app.setupRequired ? 'new-password' : 'current-password'} required /></label>
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <button class="primary" disabled={busy}>{app.setupRequired ? t('login.createAdmin') : t('login.signIn')}</button>
  </form>
</div>
