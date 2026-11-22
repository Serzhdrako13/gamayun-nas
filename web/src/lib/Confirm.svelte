<script>
  // Двухшаговая кнопка опасного действия: первый клик просит подтверждения.
  // aria — подпись для экранного диктора (если label неоднозначен в списке);
  // hint — пояснение, показывается на шаге подтверждения.
  import { t } from './i18n/index.js';
  let { label, onconfirm, disabled = false, aria = undefined, hint = '' } = $props();
  let armed = $state(false);
  let timer;
  function click() {
    if (!armed) {
      armed = true;
      clearTimeout(timer);
      timer = setTimeout(() => (armed = false), 4000);
    } else {
      armed = false;
      onconfirm();
    }
  }
</script>

<button class={armed ? 'danger' : ''} {disabled} onclick={click} aria-label={aria && (armed ? t('ui.confirmAria', { label: aria }) : aria)}>{armed ? t('ui.confirm', { label }) : label}</button>
{#if armed && hint}<span class="small muted" role="status">{hint}</span>{/if}
