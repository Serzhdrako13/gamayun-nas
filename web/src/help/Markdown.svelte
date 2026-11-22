<script>
 // рендер текста справки без вставки HTML: блоки и строки из parse.js
 // выводятся элементами Svelte (текст экранируется). help:id — onhelp(id);
 // ссылка на ненаписанный раздел — просто текст.
  import { blocks as parse } from './parse.js';
  import { hasHelp } from './content.js';
  import Self from './Markdown.svelte';
  let { source = '', tree = null, onhelp } = $props();
  const list = $derived(tree ?? parse(source));
</script>

{#snippet line(toks)}{#each toks as k}{#if k.t === 'b'}<strong>{k.v}</strong>{:else if k.t === 'code'}<code>{k.v}</code>{:else if k.t === 'help'}{#if hasHelp(k.href)}<a href={'#/help?id=' + k.href} onclick={(e) => { e.preventDefault(); onhelp?.(k.href); }}>{k.v}</a>{:else}{k.v}{/if}{:else if k.t === 'link'}<a href={k.href} target="_blank" rel="noreferrer">{k.v}</a>{:else}{k.v}{/if}{/each}{/snippet}

<div class="help-md">
  {#each list as b}
    {#if b.t === 'h3'}<h3>{@render line(b.v)}</h3>
    {:else if b.t === 'h4'}<h4>{@render line(b.v)}</h4>
    {:else if b.t === 'p'}<p>{@render line(b.v)}</p>
    {:else if b.t === 'ul'}<ul>{#each b.items as it}<li>{@render line(it)}</li>{/each}</ul>
    {:else if b.t === 'ol'}<ol>{#each b.items as it}<li>{@render line(it)}</li>{/each}</ol>
    {:else if b.t === 'pre'}<pre>{b.v}</pre>
    {:else if b.t === 'table'}
      <div class="help-tw"><table>
        <thead><tr>{#each b.head as c}<th>{@render line(c)}</th>{/each}</tr></thead>
        <tbody>{#each b.body as r}<tr>{#each r as c}<td>{@render line(c)}</td>{/each}</tr>{/each}</tbody>
      </table></div>
    {:else if b.t === 'details'}
      <details class="help-tech"><summary>{b.summary}</summary><Self tree={b.children} {onhelp} /></details>
    {/if}
  {/each}
</div>
