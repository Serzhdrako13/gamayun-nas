<script>
  import { health } from './format.js';
  let { pool } = $props();
  const h = $derived(health(pool));
  // У нового пула в начальной синхронизации — процент.
  const pct = $derived(pool.status?.initial_sync && pool.status?.sync ? ` ${Math.floor(pool.status.sync.percent)} %` : '');
</script>

<span class="badge {h[1]}">{h[0]}{pct}</span>
