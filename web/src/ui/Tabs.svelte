<script>
  // Вкладки: вкладки открытых разделов (стиль «Вкладки») и панель
  // задач (стиль «Рабочий стол»). items: [{id, label, closable?, dim?}].
  import { t } from '../lib/i18n/index.js';
  let { items, active, onselect, onclose } = $props();
</script>

<nav class="ui-tabs" aria-label={t('ui.openTabs')}>
  {#each items as tab (tab.id)}
    <span class="ui-tab" class:on={tab.id === active} class:dim={tab.dim}>
      <button class="ui-tab-b" onclick={() => onselect(tab.id)}>{tab.label}</button>
      {#if tab.closable && onclose}<button class="ui-tab-x" onclick={() => onclose(tab.id)} aria-label={t('ui.closeTab', { label: tab.label })}>×</button>{/if}
    </span>
  {/each}
</nav>
