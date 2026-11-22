<script>
  // Дерево вложенных каталогов общей папки. Каталоги догружаются
  // по раскрытию: GET /nas/shares/{имя}/dirs?path=…&depth=1.
  // Перенос в другой каталог той же общей папки — POST …/dirs/move,
  // после него дерево перечитывается целиком (reloadall от корня).
  // У существующих папок с несовместимым с Windows именем — пометка.
  // Создание, переименование и удаление (только пустых) каталогов:
  // POST/PATCH/DELETE /nas/shares/{имя}/dirs. После операции перечитывается
  // затронутая ветка: для новой папки — ветка родителя, для переименования и
  // удаления — список, где стоит каталог (onreload; у корня — от страницы).
  import { get, post, patch, del } from './api.js';
  import { flash } from './state.svelte.js';
  import DirTree from './DirTree.svelte';
  import Icon from './Icon.svelte';
  import Confirm from './Confirm.svelte';
  import { dirNameError } from './names.js';
  import { moveTargets, moveError } from './dirs.js';
  import { t } from './i18n/index.js';

  // root — верхний уровень дерева: кнопка «Новая папка» в корне общей папки.
  // reloadall — перечитать всё дерево (передаётся вниз от корня).
  let { share, nodes, onreload, reloadall, root = false } = $props();
  let gen = $state(0);
  // У корня — своя: перечитать страницу и пересоздать вложенные ветки
  // (их кэш kids устарел после переноса).
  async function reloadAll() {
    if (reloadall) return reloadall();
    if (onreload) await onreload();
    gen++;
  }
  let kids = $state({}); // path -> {nodes, partial}
  let openSet = $state({});
  let busy = $state({});
  let err = $state('');
  // Открытая форма: {mode: 'new'|'rename'|'move', path, value}; path —
  // родитель (new, "" — корень) или переименуемый/переносимый каталог;
  // для move value — путь цели, targets/incomplete — список целей.
  let form = $state(null);

  // Подсказка до отправки — те же правила, что в gamayund и helper
  // (имя обрезается по краям, как при отправке; пустое — только required).
  const uid = $props.id();
  const nameErr = $derived(form && form.mode !== 'move' && form.value.trim() ? dirNameError(form.value.trim()) : '');
  const moveErr = $derived(form?.mode === 'move' ? moveError(form.path, form.value) : '');

  const enc = encodeURIComponent;
  const childrenOf = (n) => kids[n.path]?.nodes ?? n.children;
  const url = () => `/nas/shares/${enc(share)}/dirs`;

  async function loadKids(p) {
    const r = await get(`${url()}?depth=1&path=${enc(p)}`);
    kids[p] = { nodes: r.dirs || [], partial: !!r.partial };
  }

  async function toggle(n) {
    if (openSet[n.path]) {
      openSet[n.path] = false;
      return;
    }
    openSet[n.path] = true;
    if (childrenOf(n) != null) return;
    busy[n.path] = true;
    err = '';
    try {
      await loadKids(n.path);
    } catch (e) {
      err = e.message;
      openSet[n.path] = false;
    } finally {
      busy[n.path] = false;
    }
  }

  // Понятный текст для отказов: 409 у создания/переименования — имя занято,
  // у удаления — папка не пуста.
  function message(e, mode, name) {
    if (e.status === 409) {
      if (mode === 'move') return t('dirs.conflictMove', { name });
      return mode === 'rmdir' ? t('dirs.notEmpty', { name }) : t('dirs.nameTaken', { name });
    }
    if (e.status === 404) return t('dirs.notFound');
    return e.message;
  }

  async function reloadSelf() {
    if (onreload) await onreload();
  }

  async function submit(ev) {
    ev.preventDefault();
    const f = form;
    const name = f.value.trim();
    if (!name || dirNameError(name)) return;
    err = '';
    busy[f.path] = true;
    try {
      if (f.mode === 'new') {
        await post(url(), { path: f.path, name });
        form = null;
        flash(t('dirs.created', { name }));
        if (f.path === '') await reloadSelf();
        else {
          await loadKids(f.path);
          openSet[f.path] = true;
        }
      } else {
        await patch(url(), { path: f.path, name });
        form = null;
        flash(t('dirs.renamed', { name }));
        await reloadSelf();
      }
    } catch (e) {
      err = message(e, f.mode, name);
      if (e.status === 404) await reloadSelf().catch(() => {});
    } finally {
      busy[f.path] = false;
    }
  }

  // Форма переноса — список целей из дерева до глубины 3 (как у
  // страницы); если он неполный, путь можно ввести вручную.
  async function openMove(n) {
    err = '';
    busy[n.path] = true;
    try {
      const r = await get(`${url()}?depth=3`);
      const mt = moveTargets(r.dirs || [], n.path, !!r.partial);
      form = { mode: 'move', path: n.path, name: n.name, value: mt.targets[0] ?? '', targets: mt.targets, incomplete: mt.incomplete };
    } catch (e) {
      err = e.message;
    } finally {
      busy[n.path] = false;
    }
  }

  async function submitMove(ev) {
    ev.preventDefault();
    const f = form;
    if (moveError(f.path, f.value)) return;
    err = '';
    busy[f.path] = true;
    try {
      await post(`${url()}/move`, { path: f.path, to: f.value });
      form = null;
      flash(t('dirs.moved', { name: f.name, to: f.value || t('dirs.root') }));
      await reloadAll();
    } catch (e) {
      err = message(e, 'move', f.name);
      if (e.status === 404) await reloadAll().catch(() => {});
    } finally {
      busy[f.path] = false;
    }
  }
  const label = (p) => (p === '' ? t('dirs.rootLabel') : '/' + p);

  async function remove(n) {
    err = '';
    busy[n.path] = true;
    try {
      await del(`${url()}?path=${enc(n.path)}`);
      flash(t('dirs.deleted', { name: n.name }));
      await reloadSelf();
    } catch (e) {
      err = message(e, 'rmdir', n.name);
      if (e.status === 404) await reloadSelf().catch(() => {});
    } finally {
      busy[n.path] = false;
    }
  }

  function key(e) {
    if (e.key === 'Escape') form = null;
  }
  function focus(node) {
    node.focus();
    node.select();
  }
</script>

{#snippet dirForm(label)}
  <form class="row dirform" onsubmit={submit}>
    <input use:focus bind:value={form.value} onkeydown={key} aria-label={label} aria-invalid={!!nameErr} aria-describedby={nameErr ? uid + '-err' : undefined} required />
    <button class="primary" disabled={busy[form.path] || !!nameErr}>{form.mode === 'new' ? t('common.create') : t('common.save')}</button>
    <button type="button" onclick={() => (form = null)}>{t('common.cancel')}</button>
  </form>
  {#if nameErr}<p class="error small" id={uid + '-err'} role="alert">{nameErr[0].toUpperCase() + nameErr.slice(1)}</p>{/if}
{/snippet}

{#snippet moveForm()}
  <form class="row dirform" onsubmit={submitMove}>
    <select bind:value={form.value} aria-label={t('dirs.moveWhere', { name: form.name })} aria-invalid={!!moveErr}>
      {#each form.targets as p}<option value={p}>{label(p)}</option>{/each}
      {#if form.incomplete && form.value !== '' && !form.targets.includes(form.value)}<option value={form.value}>{label(form.value)}</option>{/if}
    </select>
    {#if form.incomplete}
      <input bind:value={form.value} aria-label={t('dirs.manualPath')} placeholder={t('dirs.pathPlaceholder')} />
    {/if}
    <button class="primary" disabled={busy[form.path] || !!moveErr || (!form.targets.length && !form.incomplete)}>{t('common.move')}</button>
    <button type="button" onclick={() => (form = null)}>{t('common.cancel')}</button>
  </form>
  {#if !form.targets.length && !form.incomplete}<p class="muted small tree-empty">{t('dirs.nowhere')}</p>{/if}
  {#if form.incomplete}<p class="muted small tree-empty">{t('dirs.incomplete')}</p>{/if}
  {#if moveErr}<p class="error small tree-empty" role="alert">{moveErr[0].toUpperCase() + moveErr.slice(1)}</p>{/if}
{/snippet}

{#if root}
  <div class="row">
    <button onclick={() => (form = { mode: 'new', path: '', value: '' })} aria-label={t('dirs.newInRoot', { name: share })}>{t('common.newFolder')}</button>
  </div>
  {#if form?.mode === 'new' && form.path === ''}{@render dirForm(t('dirs.newNameInRoot', { name: share }))}{/if}
{/if}
{#if err}<p class="error" role="alert">{err}</p>{/if}
{#if root && !nodes.length}
  <p class="muted">{t('dirs.noChildrenRoot')}</p>
{/if}
{#key gen}
<ul class="tree">
  {#each nodes as n (n.path)}
    {@const ch = childrenOf(n)}
    {@const winErr = dirNameError(n.name)}
    <li>
      <div>
        <button class="link tw" aria-expanded={!!openSet[n.path]} aria-label={openSet[n.path] ? t('dirs.collapse', { name: n.name }) : t('dirs.expand', { name: n.name })} onclick={() => toggle(n)}>{openSet[n.path] ? '▾' : '▸'}</button>
        <Icon name="folder" /><span class="sp">{n.name}</span>
        {#if winErr}<button class="badge warn" title={t('dirs.winTitle', { error: winErr })} aria-label={t('dirs.winAria', { name: n.name, error: winErr })} onclick={() => (form = { mode: 'rename', path: n.path, value: n.name })}>{t('dirs.winBadge')}</button>{/if}
        {#if busy[n.path]}<span class="muted small">{t('common.loadingLower')}</span>{/if}
        <span class="row dirops">
          <button onclick={() => (form = { mode: 'new', path: n.path, value: '' })} aria-label={t('dirs.newIn', { name: n.name })}>{t('common.newFolder')}</button>
          <button onclick={() => (form = { mode: 'rename', path: n.path, value: n.name })} aria-label={t('dirs.renameAria', { name: n.name })}>{t('common.rename')}</button>
          <button onclick={() => openMove(n)} disabled={busy[n.path]} aria-label={t('dirs.moveAria', { name: n.name })}>{t('common.move')}</button>
          <Confirm label={t('common.delete')} aria={t('dirs.deleteAria', { name: n.name })} hint={t('dirs.deleteHint')} disabled={busy[n.path]} onconfirm={() => remove(n)} />
        </span>
      </div>
      {#if form && form.path === n.path}
        {#if form.mode === 'move'}{@render moveForm()}
        {:else}{@render dirForm(form.mode === 'new' ? t('dirs.newNameIn', { name: n.name }) : t('dirs.newNameFor', { name: n.name }))}{/if}
      {/if}
      {#if openSet[n.path] && ch}
        {#if ch.length}
          <DirTree {share} nodes={ch} onreload={() => loadKids(n.path)} reloadall={reloadAll} />
        {:else}
          <p class="muted small tree-empty">{t('dirs.noChildren')}</p>
        {/if}
        {#if kids[n.path]?.partial || n.partial}<p class="muted small tree-empty">{t('dirs.partial')}</p>{/if}
      {/if}
    </li>
  {/each}
</ul>
{/key}
