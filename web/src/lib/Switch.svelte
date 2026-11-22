<script>
  // Переключатель: действие применяется сразу (флаги и службы).
  // Для выбора из набора остаются чекбоксы. Вид — role="switch" на настоящем input.
  let { checked = false, onchange = () => {}, label, hint = '', disabled = false, bare = false } = $props();
</script>

{#snippet sw()}
  <span class="sw">
    <input type="checkbox" role="switch" {checked} {disabled} aria-label={bare ? label : undefined} onchange={(e) => { const v = e.currentTarget.checked; e.currentTarget.checked = checked; onchange(v); }} />
    <b></b>
  </span>
{/snippet}

{#if bare}
  {@render sw()}
{:else}
  <label class="set">
    <span class="sp"><b>{label}</b>{#if hint}<small>{hint}</small>{/if}</span>
    {@render sw()}
  </label>
{/if}
