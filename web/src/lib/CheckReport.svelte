<script>
  // Итог проверки (установка/обновление/правки из каталога):
  // запреты, конфликты портов, риски с подтверждением, изменения.
  import HelpButton from '../help/HelpButton.svelte';
  import { t } from './i18n/index.js';

  let { check, confirm, ontoggle } = $props();
</script>

{#if check.forbidden?.length}
  <div class="banner bad"><b>{t('apps.editor.forbidden')}</b>
    <ul>{#each check.forbidden as i}<li>{#if i.service}<b>{i.service}</b>: {/if}{i.detail}</li>{/each}</ul></div>
{/if}
{#if check.conflicts?.length}
  <div class="banner bad"><b>{t('apps.editor.conflicts')}</b> <HelpButton section="apps.ports" />
    <ul>{#each check.conflicts as i}<li>{i.detail}</li>{/each}</ul></div>
{/if}
{#if check.risks?.length}
  <div class="banner warn stack"><b>{t('apps.editor.risks')}</b> <HelpButton section="apps.risks" />
    {#each check.risks as r}
      <label class="row"><input type="checkbox" checked={confirm.has(r.key)} onchange={(e) => ontoggle(r.key, e.currentTarget.checked)} />
        {#if r.service}<b>{r.service}</b>:{/if} {r.detail}</label>
    {/each}</div>
{/if}
{#if check.changes?.length}
  <details><summary>{t('apps.editor.changes')} ({check.changes.length})</summary>
    <ul class="small">{#each check.changes as i}<li>{#if i.service}<b>{i.service}</b>: {/if}{i.detail}</li>{/each}</ul></details>
{/if}
{#if check.normalized}
  <details><summary>{t('apps.editor.normalized')}</summary><pre class="mono small">{check.normalized}</pre></details>
{/if}
