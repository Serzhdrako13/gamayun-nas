<script>
  // Страница «Справка» — оглавление с поиском и раздел. Адрес
  // #/help?id=<раздел> (route.js часть после «?» не учитывает — читаем здесь).
  import { onMount } from 'svelte';
  import Markdown from '../help/Markdown.svelte';
  import { helpSection, helpSections, searchHelp } from '../help/content.js';
  import { t } from '../lib/i18n/index.js';

  const idOf = () => new URLSearchParams((location.hash.split('?')[1]) || '').get('id') || 'overview';
  let id = $state(idOf());
  let query = $state('');
  const sec = $derived(helpSection(id));
  const list = $derived(query ? searchHelp(query) : helpSections());
  const depth = (s) => s.id.split('.').length - 1;

  onMount(() => {
    const sync = () => {
      if (location.hash.replace(/^#\/?/, '').split('?')[0] === 'help') id = idOf();
    };
    addEventListener('hashchange', sync);
    return () => removeEventListener('hashchange', sync);
  });
  const go = (x) => (location.hash = '#/help?id=' + encodeURIComponent(x));
</script>

<div class="help-page">
  <nav class="help-toc" aria-label={t('help.contents')}>
    <input type="search" bind:value={query} placeholder={t('help.search')} aria-label={t('help.search')} />
    {#if list.length}
      <ul>
        {#each list as s}
          <li class:on={s.id === id} class={'d' + Math.min(depth(s), 3)}><a href={'#/help?id=' + s.id}>{s.title}</a></li>
        {/each}
      </ul>
    {:else}<p class="muted small">{t('help.nothing')}</p>{/if}
  </nav>
  <article class="help-art">
    {#if sec}
      <h2 class="help-h">{sec.title}</h2>
      <Markdown source={sec.body} onhelp={go} />
    {:else}<p class="muted">{t('help.noSection')}</p>{/if}
  </article>
</div>
