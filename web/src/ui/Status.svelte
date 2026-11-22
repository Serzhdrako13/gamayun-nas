<script>
  // Метка состояния. kind: ok | warn | bad | info | free.
  let { kind = 'info', children } = $props();
</script>

<span class="ui-status {kind}">{@render children?.()}</span>
