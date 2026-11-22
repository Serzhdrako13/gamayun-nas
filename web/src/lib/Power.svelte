<script>
  // Выключение и перезагрузка: кнопка ⏻ в верхней панели →
  // меню → окно подтверждения с явным действием. После команды — заставка;
  // при перезагрузке страница ждёт возврата сервера и обновляется сама.
  import { post, get } from './api.js';
  import { POWER } from '../shell/power.js';
  import { t } from './i18n/index.js';

  let open = $state(false);
  let ask = $state(''); // действие на подтверждении
  let busy = $state(false);
  let err = $state('');
  let done = $state('');
  let root = $state();

  // Окна — в body: у панели есть backdrop-filter, иначе fixed считается от неё.
  function portal(node) { document.body.appendChild(node); return { destroy() { node.remove(); } }; }
  function outside(e) { if (open && root && !root.contains(e.target)) open = false; }
  function key(e) { if (e.key === 'Escape') { if (ask && !busy) ask = ''; else open = false; } }

  async function run() {
    busy = true; err = '';
    try {
      await post('/system/power', { action: ask, confirm: ask });
      done = ask; ask = '';
      if (done === 'reboot') wait();
    } catch (e) {
      err = e.message;
    } finally {
      busy = false;
    }
  }
  // Ждём, пока сервер пропадёт и вернётся (до 15 мин), затем обновляем страницу.
  async function wait() {
    let down = false;
    for (let i = 0; i < 180; i++) {
      await new Promise((r) => setTimeout(r, 5000));
      try { await get('/health'); if (down) { location.reload(); return; } } catch { down = true; }
    }
  }
</script>

<svelte:window onpointerdown={outside} onkeydown={key} />
<span class="pw" bind:this={root}>
  <button class="pw-b" onclick={() => (open = !open)} aria-haspopup="menu" aria-expanded={open} aria-label={t('power.aria')} title={t('power.title')}>⏻</button>
  {#if open}
    <div class="pw-m glass" role="menu">
      {#each Object.entries(POWER) as [a, p] (a)}
        <button role="menuitem" onclick={() => { open = false; ask = a; err = ''; }}>{p.label}…</button>
      {/each}
    </div>
  {/if}
</span>
{#if ask}<div use:portal>
  <div class="scrim pw-s" role="presentation" onclick={() => { if (!busy) ask = ''; }}></div>
  <div class="pw-d glass" role="alertdialog" aria-modal="true" aria-labelledby="pw-q" aria-describedby="pw-w">
    <h2 id="pw-q">{POWER[ask].q}</h2>
    <p id="pw-w" class="muted">{t('power.warn')}</p>
    {#if err}<p class="error" role="alert">{err}</p>{/if}
    <div class="row pw-a">
      <button onclick={() => (ask = '')} disabled={busy}>{t('common.cancel')}</button>
      <button class="danger" onclick={run} disabled={busy}>{busy ? t('power.sending') : POWER[ask].label}</button>
    </div>
  </div>
</div>{/if}
{#if done}<div use:portal>
  <div class="scrim pw-s"></div>
  <div class="pw-d glass" role="status"><h2>{POWER[done].done}</h2>{#if done === 'reboot'}<p class="muted">{t('power.reloadHint')}</p>{/if}</div>
</div>{/if}

<style>
  .pw { position: relative; }
  .pw-b { min-width: 34px; padding: 0 8px; font-size: 1.05rem; }
  .pw-m { position: absolute; right: 0; top: calc(100% + 6px); z-index: 100001; display: flex; flex-direction: column; gap: 4px; padding: 6px; min-width: 170px; background: var(--solid); }
  .pw-m button { text-align: left; }
  .pw-d { position: fixed; z-index: 100003; left: 50%; top: 20vh; transform: translateX(-50%); width: min(440px, 92vw); padding: 18px 20px; background: var(--solid); border-radius: var(--radius); }
  .pw-d h2 { margin: 0 0 8px; font-size: 1.15rem; }
  .pw-a { justify-content: flex-end; gap: 8px; margin-top: 12px; }
  .pw-s { z-index: 100002; }
</style>
