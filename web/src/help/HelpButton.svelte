<script>
 // «?» у блока — открывает свой раздел справки поверх страницы.
 // Раздел не написан — кнопки нет.
  import { hasHelp } from './content.js';
  import { openHelp } from './help.svelte.js';
  import { t } from '../lib/i18n/index.js';
  let { section, label = '' } = $props();
</script>

{#if hasHelp(section)}
  <button
    type="button"
    class="help-btn"
    title={label || t('help.open')}
    aria-label={label || t('help.open')}
    onclick={(e) => {
      e.preventDefault();
      e.stopPropagation();
      openHelp(section);
    }}>?</button>
{/if}
