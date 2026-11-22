<script>
  // Стиль «Рабочий стол»: слева виджеты, на столе ярлыки разделов,
  // «Меню» — все разделы; разделы — плавающие окна (перетаскивание за
  // заголовок, свернуть, развернуть, двойной щелчок — развернуть), сверху —
  // панель задач. Положение окон — на пользователя (браузер, ключ с именем).
  import { onMount } from 'svelte';
  import { app } from '../lib/state.svelte.js';
  import Icon from '../lib/Icon.svelte';
  import Tabs from '../ui/Tabs.svelte';
  import Window from '../ui/Window.svelte';
  import Widgets from './Widgets.svelte';
  import { SECTIONS, section } from './sections.js';
  import { t } from '../lib/i18n/index.js';
  import { VIEWS } from './views.js';
  import { parseRoute, routeOf } from './route.js';

  let { topright } = $props();
  const KEY = 'dn.desk.' + (app.session?.user?.name || '');
  const saved = (() => { try { return JSON.parse(localStorage.getItem(KEY)) || {}; } catch { return {}; } })();
  // win[id] = {x, y, w, h, z, min, max, nav}; open — в порядке открытия (панель задач)
  let win = $state(Object.fromEntries(Object.entries(saved.win || {}).filter(([id]) => section(id) && !section(id).soon)));
  let order = $state((saved.order || []).filter((id) => win[id]));
  let top = $state(null);
  let menu = $state(false);
  let deskEl = $state();
  let Z = Math.max(10, ...Object.values(win).map((w) => w.z || 0));

  $effect(() => { try { localStorage.setItem(KEY, JSON.stringify({ win, order })); } catch { /* приватный режим */ } });

  function place(id) {
    const n = order.length, W = deskEl?.clientWidth || 900, H = deskEl?.clientHeight || 600;
    return { x: 110 + (n % 6) * 28, y: 12 + (n % 6) * 24, w: Math.max(420, Math.min(860, W - 140)), h: Math.max(320, Math.min(560, H - 40)), z: ++Z, nav: 0 };
  }
  // Окно целиком в пределах стола (ниже верхней панели) —
  // сохранённые размеры с большего экрана подрезаются.
  function fit(id) {
    const w = win[id], W = deskEl?.clientWidth, H = deskEl?.clientHeight;
    if (!W || !H) return;
    w.h = Math.max(240, Math.min(w.h, H));
    w.y = Math.max(0, Math.min(w.y, H - w.h));
    w.w = Math.max(320, Math.min(w.w, W));
  }
  function front(id) { win[id].z = ++Z; win[id].min = false; top = id; fit(id); }
  function open(id, nav) {
    menu = false;
    if (!win[id]) { win[id] = place(id); order.push(id); }
    if (nav != null) win[id].nav = nav;
    front(id);
    const h = routeOf(id, win[id].nav);
    if (location.hash.split('?')[0] !== h) history.replaceState(null, '', h);
  }
  function close(id) { delete win[id]; order.splice(order.indexOf(id), 1); if (top === id) top = null; }
  function task(id) { if (top === id && !win[id].min) { win[id].min = true; top = null; } else front(id); }

  function sync() { const r = parseRoute(location.hash); if (r.section !== 'home') open(r.section, r.nav); }
  onMount(() => { sync(); addEventListener('hashchange', sync); return () => removeEventListener('hashchange', sync); });

  let drag = null;
  function dragstart(id, e) {
    if (e.button !== 0 || e.target.closest('button') || win[id].max || matchMedia('(max-width: 640px)').matches) return;
    front(id);
    drag = { id, sx: e.clientX, sy: e.clientY, x: win[id].x, y: win[id].y };
    e.currentTarget.setPointerCapture(e.pointerId);
    e.currentTarget.onpointermove = (ev) => {
      if (!drag) return;
      const W = deskEl.clientWidth, H = deskEl.clientHeight;
      win[id].x = Math.max(-win[id].w + 120, Math.min(W - 80, drag.x + ev.clientX - drag.sx));
      win[id].y = Math.max(0, Math.min(H - 40, drag.y + ev.clientY - drag.sy));
    };
    e.currentTarget.onpointerup = (ev) => { drag = null; ev.currentTarget.onpointermove = null; };
  }
  const items = $derived(order.map((id) => ({ id, label: section(id).title, dim: win[id].min })));
  const shortcuts = SECTIONS.filter((s) => s.shortcut && !s.soon);
</script>

<!-- Меню закрывается по Esc и по щелчку вне меню и кнопки «Меню». -->
<svelte:window onkeydown={(e) => { if (e.key === 'Escape') menu = false; }}
  onpointerdown={(e) => { if (menu && !e.target.closest?.('.sh-popmenu, .sh-menu')) menu = false; }} />
<div class="sh">
  <header class="sh-bar glass">
    <button class="sh-menu" onclick={() => (menu = !menu)} aria-expanded={menu}>{t('ui.menu')}</button>
    <Tabs {items} active={top} onselect={task} />
    <span class="ui-sp"></span>
    {@render topright?.()}
  </header>
  <div class="sh-lay">
    <Widgets onopen={(id) => open(id)} />
    <main class="sh-desk" bind:this={deskEl}>
      <div class="sh-icons">
        {#each shortcuts as s (s.id)}
          <button class="sh-sc" ondblclick={() => open(s.id)} onclick={(e) => { if (e.detail === 0 || matchMedia('(pointer: coarse)').matches) open(s.id); }}
            title={t('ui.openDblClick')}><span class="ui-ico"><Icon name={s.icon} /></span><b>{s.title}</b></button>
        {/each}
      </div>
      {#each order as id (id)}
        {@const w = win[id]}
        {@const s = section(id)}
        {@const View = VIEWS[s.nav[w.nav]?.view]}
        <div class="sh-fw" class:max={w.max} hidden={w.min} style:left={w.x + 'px'} style:top={w.y + 'px'} style:width={w.w + 'px'} style:height={w.h + 'px'} style:z-index={w.z}
          role="presentation" onpointerdown={() => { if (top !== id) front(id); }}>
          <Window title={s.title} icon={s.icon} help={s.id} nav={s.nav.map((n) => n.label)} active={w.nav}
            onnav={(i) => open(id, i)} onclose={() => close(id)} onmin={() => { w.min = true; top = null; }} onmax={() => (w.max = !w.max)}
            dragstart={(e) => dragstart(id, e)}>
            {#if View}{#key w.nav}<View />{/key}{/if}
          </Window>
        </div>
      {/each}
      {#if menu}
        <div class="sh-popmenu glass" role="dialog" aria-label={t('ui.allSections')}>
          {#each SECTIONS as sec (sec.id)}
            <button class="sh-tile" disabled={sec.soon} onclick={() => open(sec.id)}><span class="ui-ico"><Icon name={sec.icon} /></span><b>{sec.title}</b><small>{sec.desc}</small></button>
          {/each}
        </div>
      {/if}
    </main>
  </div>
</div>
