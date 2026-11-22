<script>
  import { app } from './state.svelte.js';
  import { t } from './i18n/index.js';
  let { items, onclose } = $props();
  let q = $state('');
  let sel = $state(0);
  const all = $derived([
    ...items.map((n) => ({ label: t('app.palette.go', { label: n.label }), go: n.path })),
    { label: t('app.palette.createPool'), go: '/pools?new' },
    ...app.pools.map((p) => ({ label: t('app.palette.pool', { name: p.name }), go: '/pools?open=' + p.name })),
  ]);
  const shown = $derived(all.filter((a) => a.label.toLowerCase().includes(q.toLowerCase())));
  function run(a) {
    if (!a) return;
    location.hash = '#' + a.go;
    onclose();
  }
  function key(e) {
    if (e.key === 'Escape') onclose();
    else if (e.key === 'ArrowDown') (sel = Math.min(sel + 1, shown.length - 1)), e.preventDefault();
    else if (e.key === 'ArrowUp') (sel = Math.max(sel - 1, 0)), e.preventDefault();
    else if (e.key === 'Enter') run(shown[sel]);
  }
  function focus(node) {
    node.focus();
  }
</script>

<div class="scrim" onclick={onclose} role="presentation"></div>
<div class="palette" role="dialog" aria-label={t('app.palette.aria')}>
  <input use:focus bind:value={q} oninput={() => (sel = 0)} onkeydown={key} placeholder={t('app.palette.placeholder')} />
  <ul>
    {#each shown as a, i}
      <li class:sel={i === sel}><button onclick={() => run(a)}>{a.label}</button></li>
    {/each}
  </ul>
</div>
