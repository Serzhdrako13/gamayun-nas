<script>
  // Полоса заполнения: value из max; kind — цвет по состоянию.
  let { value = 0, max = 100, kind = '' } = $props();
  const w = $derived(max > 0 ? Math.max(0, Math.min(100, (value / max) * 100)) : 0);
</script>

<div class="ui-meter {kind}" role="meter" aria-valuemin="0" aria-valuemax={max} aria-valuenow={value}><i style:width={w + '%'}></i></div>
