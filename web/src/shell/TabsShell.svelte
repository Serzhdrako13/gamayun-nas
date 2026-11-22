<script>
  // Стиль «Вкладки»: слева виджеты, сверху «Главная» и вкладки
  // открытых разделов, раздел — окно во всю рабочую зону со своим меню.
  // Вкладки — из манифестов (sections.js); открытые запоминаются в браузере.
  import { onMount } from 'svelte';
  import { app } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Tabs from '../ui/Tabs.svelte';
  import Window from '../ui/Window.svelte';
  import Widgets from './Widgets.svelte';
  import Home from '../routes/Home.svelte';
  import { SECTIONS, section } from './sections.js';
  import { t } from '../lib/i18n/index.js';
  import { VIEWS } from './views.js';
  import { parseRoute, routeOf } from './route.js';

  let { topright } = $props();
  const KEY = 'dn.tabs.open';
  const load = () => { try { return JSON.parse(localStorage.getItem(KEY)) || []; } catch { return []; } };
  let open = $state(load().filter((id) => section(id) && !section(id).soon));
  let cur = $state('home');
  let nav = $state({});

  function sync() {
    const r = parseRoute(location.hash);
    if (r.section !== 'home' && !open.includes(r.section)) open.push(r.section);
    cur = r.section;
    if (r.section !== 'home') nav[r.section] = r.nav;
  }
  onMount(() => { sync(); addEventListener('hashchange', sync); return () => removeEventListener('hashchange', sync); });
  $effect(() => { try { localStorage.setItem(KEY, JSON.stringify(open)); } catch { /* приватный режим */ } });

  const go = (id, i = nav[id] || 0) => { const h = routeOf(id, i); if (location.hash !== h) location.hash = h; else sync(); };
  function close(id) {
    const i = open.indexOf(id);
    open.splice(i, 1);
    if (cur === id) go(open[i - 1] || open[i] || 'home');
  }
  const items = $derived([{ id: 'home', label: t('ui.home') }, ...open.map((id) => ({ id, label: section(id).title, closable: true }))]);
  const s = $derived(cur === 'home' ? null : section(cur));
  const View = $derived(s ? VIEWS[s.nav[nav[cur] || 0]?.view] : null);
</script>

<div class="sh">
  <header class="sh-bar glass">
    <span class="sh-brand">{app.product}</span>
    <Tabs {items} active={cur} onselect={(id) => go(id)} onclose={close} />
    <span class="ui-sp"></span>
    {@render topright?.()}
  </header>
  <div class="sh-lay">
    <Widgets onopen={(id) => go(id)} />
    <main class="sh-work">
      {#if !s}
        <div class="sh-tiles">
          {#each SECTIONS as sec (sec.id)}
            <button class="sh-tile glass" disabled={sec.soon} onclick={() => go(sec.id)}>
              <span class="ui-ico"><Icon name={sec.icon} /></span><b>{sec.title}</b><small>{sec.desc}</small>
            </button>
          {/each}
        </div>
        <Home />
      {:else}
        {#key cur}
          <Window title={s.title} icon={s.icon} help={s.id} nav={s.nav.map((n) => n.label)} active={nav[cur] || 0} onclose={() => close(cur)}
            onnav={(i) => go(cur, i)}>
            {#if View}{#key nav[cur]}<View />{/key}{/if}
          </Window>
        {/key}
      {/if}
    </main>
  </div>
</div>
