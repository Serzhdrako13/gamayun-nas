<script>
  // Свойства «название — значение». items: [[название, значение]];
  // значение-сниппет — для ссылок и меток.
  let { items } = $props();
</script>

<dl class="ui-props">
  {#each items as [k, v]}<dt>{k}</dt><dd>{#if typeof v === 'function'}{@render v()}{:else}{v ?? '—'}{/if}</dd>{/each}
</dl>
