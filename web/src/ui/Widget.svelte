<script>
  // Плашка колонки виджетов. onclick — вся плашка ведёт в раздел;
  // onhead — ссылкой служит только заголовок (внутри есть свои кнопки).
  let { title = '', onclick, onhead, children, extra } = $props();
</script>

{#if onclick}
  <button class="ui-widget glass link" {onclick}>
    {#if title}<span class="ui-widget-h"><b>{title}</b>{#if extra}{@render extra()}{/if}</span>{/if}
    {@render children?.()}
  </button>
{:else}
  <div class="ui-widget glass">
    {#if title}<div class="ui-widget-h">{#if onhead}<button class="ui-widget-t" onclick={onhead}>{title} ›</button>{:else}<b>{title}</b>{/if}{#if extra}{@render extra()}{/if}</div>{/if}
    {@render children?.()}
  </div>
{/if}
