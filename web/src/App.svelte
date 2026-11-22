<script>
  import { onMount } from 'svelte';
  import { app, loadSession, startPolling, stopPolling, initTheme, setTheme, setLang } from './lib/state.svelte.js';
  import { post } from './lib/api.js';
  import Login from './routes/Login.svelte';
  import Wizard from './routes/Wizard.svelte';
  import Palette from './lib/Palette.svelte';
  import Power from './lib/Power.svelte';
  import TabsShell from './shell/TabsShell.svelte';
  import DesktopShell from './shell/DesktopShell.svelte';
  import { SECTIONS } from './shell/sections.js';
  import { t, locale } from './lib/i18n/index.js';
  import HelpWindow from './help/HelpWindow.svelte';

  // Разделы — из манифестов оболочки; палитра Ctrl+K ведёт в них.
  const PALETTE = $derived(SECTIONS.filter((s) => !s.soon).map((s) => ({ label: s.title, path: '/' + s.id })));
  let palette = $state(false);
  // Стиль оболочки — «Настройки → Вид».
  const Shell = $derived(app.shell === 'desktop' ? DesktopShell : TabsShell);

  onMount(() => {
    initTheme();
    loadSession();
    const kd = (e) => {
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        palette = !palette;
      }
    };
    window.addEventListener('keydown', kd);
    return () => {
      window.removeEventListener('keydown', kd);
    };
  });

  $effect(() => {
    if (app.session) startPolling();
    else stopPolling();
  });
  // Вход в приложение через прокси — после входа в NAS вернуться к app-login.
  $effect(() => {
    if (!app.session) return;
    const next = new URLSearchParams(location.search).get('app_login') || '';
    if (next.startsWith('/api/v1/auth/app-login?')) location.replace(next);
  });

  async function logout() {
    try {
      await post('/auth/logout');
    } catch {
      /* сессия уже недействительна */
    }
    app.session = null;
  }
  function nextTheme() {
    setTheme({ auto: 'light', light: 'dark', dark: 'auto' }[app.theme]);
  }
  // Язык — как тема: кнопка в панели и «Настройки → Вид».
  function nextLang() {
    setLang(app.lang === 'ru' ? 'en' : 'ru');
  }
</script>

{#if !app.ready}
  <p class="muted login">{t('common.loading')}</p>
{:else if !app.session}
  <Login />
{:else if app.setup && !app.setup.done}
  <Wizard />
{:else}
  {#key app.shell}
  <Shell>
    {#snippet topright()}
      <span class="sh-top">
        <span class="small muted hide-mobile">{#if app.poolsError}{t('app.stale', { error: app.poolsError })}{:else if app.updatedAt}{t('app.updated', { time: app.updatedAt.toLocaleTimeString(locale()) })}{/if}</span>
        {#if app.unread}<a class="badge bad" href="#/notify" title={t('app.unreadTitle')}>{t('app.unread', { count: app.unread })}</a>{/if}
        <button class="hide-mobile" onclick={() => (palette = true)}>{t('app.search')}</button>
        <button class="hide-mobile" onclick={nextTheme}>{t('app.theme.' + app.theme)}</button>
        <button class="hide-mobile" onclick={nextLang}>{t('app.langButton')}</button>
        <Power />
        <button onclick={logout}>{t('app.logout', { name: app.session.user.name })}</button>
      </span>
    {/snippet}
  </Shell>
  {/key}
  <!-- Справка поверх страниц, вне {#key} — переживает смену стиля оболочки. -->
  <HelpWindow />
  {#if palette}<Palette items={PALETTE} onclose={() => (palette = false)} />{/if}
{/if}
{#if app.toast}<div class="toast" role="status">{app.toast}</div>{/if}
