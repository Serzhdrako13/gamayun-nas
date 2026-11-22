<script>
  // Кольцо загрузки: value 0..100, текст в центре, подпись снизу.
  let { value = 0, text, label = '' } = $props();
  const p = $derived(Math.max(0, Math.min(100, Number(value) || 0)));
</script>

<div class="ui-ring-w">
  <div class="ui-ring" style:--p={p} role="img" aria-label="{label} {text ?? p + '%'}"><span>{text ?? p + '%'}</span></div>
  {#if label}<small>{label}</small>{/if}
</div>
