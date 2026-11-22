<script>
  // Таблица. columns: [{key, label, narrow?: false — скрыть на узком, mono?}],
  // rows — объекты; onrow — строка кликабельна; cell — свой вывод ячейки.
  import { t } from '../lib/i18n/index.js';
  // empty не задан — «Пусто» из словаря.
  let { columns, rows, onrow, cell, empty, key = (r, i) => i } = $props();
</script>

<div class="ui-table-wrap">
  <table class="ui-table">
    <thead><tr>{#each columns as c}<th class:hide-narrow={c.narrow === false}>{c.label}</th>{/each}</tr></thead>
    <tbody>
      {#each rows as r, i (key(r, i))}
        <tr class:click={!!onrow} onclick={onrow ? () => onrow(r) : undefined} tabindex={onrow ? 0 : undefined}
          onkeydown={onrow ? (e) => (e.key === 'Enter' ? onrow(r) : null) : undefined}>
          {#each columns as c}
            <td class:mono={c.mono} class:hide-narrow={c.narrow === false}>{#if cell}{@render cell(r, c)}{:else}{r[c.key] ?? '—'}{/if}</td>
          {/each}
        </tr>
      {:else}
        <tr><td class="muted" colspan={columns.length}>{empty ?? t('ui.empty')}</td></tr>
      {/each}
    </tbody>
  </table>
</div>
