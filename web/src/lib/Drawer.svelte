<script>
  import { t } from './i18n/index.js';
  let { title, onclose, children } = $props();
  function key(e) {
    // Esc, уже обработанный справкой поверх, карточку не закрывает.
    if (e.key === 'Escape' && !e.defaultPrevented) onclose();
  }
</script>

<svelte:window onkeydown={key} />
<div class="scrim" onclick={onclose} role="presentation"></div>
<div class="drawer" role="dialog" aria-modal="true" aria-label={title}>
  <header>
    <h2>{title}</h2>
    <button onclick={onclose} aria-label={t('common.close')}>{t('common.close')}</button>
  </header>
  {@render children()}
</div>
