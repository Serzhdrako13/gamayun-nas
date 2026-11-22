<script>
  // Сегментированная кнопка: один вариант из 2–4 (нет / чтение / запись, тема).
  let { options, value, onchange, label = '' } = $props();
</script>

<div class="seg" role="group" aria-label={label}>
  {#each options as [k, text]}
    <button type="button" aria-pressed={value === k} onclick={() => onchange(k)}>{text}</button>
  {/each}
</div>
