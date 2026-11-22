<script>
 // окно справки поверх всех страниц и окон (z-index 100010, help.css).
 // Один экземпляр в App.svelte вне {#key app.shell} — переживает смену стиля.
 // Свёрнутое — плашка внизу справа; «□» — во весь экран; «⧉» — открыть страницей.
 // Телефон (≤ 640 px) — панель снизу, без перетаскивания. Положение и размер —
 // через style: (CSSOM): CSP gamayund запрещает inline-стили.
  import { onMount, tick } from 'svelte';
  import Window from '../ui/Window.svelte';
  import Markdown from './Markdown.svelte';
  import { helpSection, searchHelp } from './content.js';
  import { help, goTo, back, minimize, restore, toggleMax, close, moveTo, resizeTo } from './help.svelte.js';
  import { t } from '../lib/i18n/index.js';

  let el = $state(null);
  let body = $state(null);
  let query = $state('');
  let phone = $state(false);
  const sec = $derived(helpSection(help.section));
  const found = $derived(query ? searchHelp(query) : []);
  const free = $derived(!help.max && !phone);

 // fit — окно целиком на экране (после смены размера окна браузера или загрузки).
  function fit() {
    phone = matchMedia('(max-width: 640px)').matches;
    const W = innerWidth;
    const H = innerHeight;
    if (help.w != null) help.w = Math.max(280, Math.min(help.w, W - 16));
    if (help.h != null) help.h = Math.max(200, Math.min(help.h, H - 16));
    if (help.x != null) help.x = Math.max(0, Math.min(help.x, W - (help.w ?? 420)));
    if (help.y != null) help.y = Math.max(0, Math.min(help.y, H - 60));
  }
  onMount(() => {
    fit();
    addEventListener('resize', fit);
    return () => removeEventListener('resize', fit);
  });

 // Размер (CSS resize) запоминается, пока окно не во весь экран и не на телефоне.
  $effect(() => {
    if (!el || !free) return;
    const ro = new ResizeObserver(() => {
      const w = Math.round(el.offsetWidth);
      const h = Math.round(el.offsetHeight);
      if (w && h && (w !== help.w || h !== help.h)) resizeTo(w, h);
    });
    ro.observe(el);
    return () => ro.disconnect();
  });

 // Новый раздел — с начала.
  $effect(() => {
    help.section;
    tick().then(() => body?.querySelector('.ui-win-c')?.scrollTo(0, 0));
  });

  function dragstart(e) {
    if (!free || e.button !== 0 || e.target.closest('button, input, a')) return;
    const r = el.getBoundingClientRect();
    const dx = e.clientX - r.left;
    const dy = e.clientY - r.top;
    const h = e.currentTarget;
    h.setPointerCapture(e.pointerId);
    const mv = (ev) => moveTo(Math.max(0, Math.min(innerWidth - 120, ev.clientX - dx)), Math.max(0, Math.min(innerHeight - 40, ev.clientY - dy)));
    const up = () => {
      h.removeEventListener('pointermove', mv);
      h.removeEventListener('pointerup', up);
      h.removeEventListener('pointercancel', up);
    };
    h.addEventListener('pointermove', mv);
    h.addEventListener('pointerup', up);
    h.addEventListener('pointercancel', up);
  }

 // Esc — только когда фокус внутри справки; дальше не идёт, чтобы не закрыть
 // заодно карточку пула (Drawer проверяет defaultPrevented).
  function key(e) {
    if (e.key !== 'Escape') return;
    e.preventDefault();
    e.stopPropagation();
    if (query) query = '';
    else minimize();
  }

  function asPage() {
    minimize();
    location.hash = '#/help?id=' + encodeURIComponent(help.section);
  }
  function pick(id) {
    query = '';
    goTo(id);
  }
</script>

{#if help.open}
  {#if help.min}
    <button class="help-pill" onclick={restore} title={t('help.restore')}>? {t('help.title')}: {sec?.title ?? ''} ▴</button>
  {:else}
 <!-- svelte-ignore a11y_no_noninteractive_element_interactions -->
    <div
      bind:this={el}
      class="help-win"
      class:max={help.max}
      class:placed={free && help.x != null}
      role="dialog"
      aria-label={t('help.title')}
      tabindex="-1"
      onkeydown={key}
      style:left={free && help.x != null ? help.x + 'px' : null}
      style:top={free && help.y != null ? help.y + 'px' : null}
      style:width={free && help.w != null ? help.w + 'px' : null}
      style:height={free && help.h != null ? help.h + 'px' : null}
    >
      <div class="help-body" bind:this={body}>
        <Window title={'? ' + t('help.title')} onmin={minimize} onmax={phone ? null : toggleMax} onclose={close} {dragstart}>
          {#snippet actions()}
            <button class="ui-wb" onclick={asPage} title={t('help.openPage')} aria-label={t('help.openPage')}>⧉</button>
          {/snippet}
          <div class="help-tools">
            <button onclick={back} disabled={!help.history.length}>← {t('help.back')}</button>
            <input type="search" bind:value={query} placeholder={t('help.search')} aria-label={t('help.search')} />
          </div>
          {#if query}
            {#if found.length}
              <ul class="help-found">{#each found as r}<li><a href={'#/help?id=' + r.id} onclick={(e) => { e.preventDefault(); pick(r.id); }}>{r.title}</a></li>{/each}</ul>
            {:else}<p class="muted">{t('help.nothing')}</p>{/if}
          {:else if sec}
            <h2 class="help-h">{sec.title}</h2>
            <Markdown source={sec.body} onhelp={goTo} />
          {:else}
            <p class="muted">{t('help.noSection')}</p>
          {/if}
        </Window>
      </div>
    </div>
  {/if}
{/if}
