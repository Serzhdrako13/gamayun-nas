<script>
  // Окно раздела: заголовок со значком, кнопки окна, внутреннее меню
  // раздела слева и содержимое. Одно и то же окно — во всю зону (стиль
  // «Вкладки») или плавающее (стиль «Рабочий стол»).
  import Icon from '../lib/Icon.svelte';
  import { t } from '../lib/i18n/index.js';
  import HelpButton from '../help/HelpButton.svelte';
  let { title, icon = '', help = '', nav = [], active = $bindable(0), onnav, onclose, onmin, onmax, dragstart, children, actions } = $props();
</script>

<section class="ui-win">
  <!-- Перетаскивание — только мышью/касанием; с клавиатуры окном управляют кнопки «свернуть/развернуть». -->
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <header class="ui-win-h" onpointerdown={dragstart} ondblclick={(e) => { if (onmax && !e.target.closest('button')) onmax(); }}>
    {#if icon}<span class="ui-ico sm"><Icon name={icon} /></span>{/if}
    <b>{title}</b>
    {#if help}<HelpButton section={help} />{/if}
    <span class="ui-sp"></span>
    {#if actions}{@render actions()}{/if}
    {#if onmin}<button class="ui-wb" onclick={onmin} aria-label={t('ui.minimize')}>–</button>{/if}
    {#if onmax}<button class="ui-wb" onclick={onmax} aria-label={t('ui.maximize')}>□</button>{/if}
    {#if onclose}<button class="ui-wb" onclick={onclose} aria-label={t('common.close')}>×</button>{/if}
  </header>
  <div class="ui-win-b">
    {#if nav.length}
      <nav class="ui-win-nav">
        {#each nav as n, i}<button class:on={i === active} onclick={() => (onnav ? onnav(i) : (active = i))}>{n}</button>{/each}
      </nav>
    {/if}
    <div class="ui-win-c">{@render children?.()}</div>
  </div>
</section>
