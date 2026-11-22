<script>
  import HelpButton from '../help/HelpButton.svelte';
  import { onMount } from 'svelte';
  import { app, flash } from '../lib/state.svelte.js';
  import { get, post, put, del } from '../lib/api.js';
  import Drawer from '../lib/Drawer.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import Seg from '../lib/Seg.svelte';
  import Icon from '../lib/Icon.svelte';
  import DirTree from '../lib/DirTree.svelte';
  import Snapshots from '../lib/Snapshots.svelte';
  import { winReservedName } from '../lib/names.js';
  import { t } from '../lib/i18n/index.js';

  const LEVELS = $derived([['none', t('shares.levels.none')], ['read', t('shares.levels.read')], ['rw', t('shares.levels.rw')]]);
  let shares = $state([]);
  let users = $state([]);
  let groups = $state([]);
  let err = $state('');
  let ns = $state({ name: '', pool: '', comment: '' });
  let edit = $state(null); // {name, rows: [{kind, subject, level}]}
  let snaps = $state(''); // папка, чьи снимки открыты
  // Вложенные каталоги выбранной корневой папки.
  let selName = $state('');
  let tree = $state(null); // {share, dirs, partial} | {share, error}
  const selected = $derived(shares.find((x) => x.name === selName) || null);
  async function pick(name) {
    selName = name;
    tree = null;
    try {
      const r = await get(`/nas/shares/${encodeURIComponent(name)}/dirs?depth=2`);
      if (selName === name) tree = { share: name, dirs: r.dirs || [], partial: !!r.partial };
    } catch (e) {
      if (selName === name) tree = { share: name, error: e.message };
    }
  }

  // Перечитать корень дерева без сброса.
  async function refresh() {
    const name = selName;
    const r = await get(`/nas/shares/${encodeURIComponent(name)}/dirs?depth=2`);
    if (selName === name) tree = { share: name, dirs: r.dirs || [], partial: !!r.partial };
  }

  async function load() {
    try {
      const [s, u, g] = await Promise.all([get('/nas/shares'), get('/nas/users'), get('/nas/groups')]);
      shares = s.shares || [];
      users = u.users || [];
      groups = g.groups || [];
      if (!selName && shares.length) pick(shares[0].name);
      else if (selName && !shares.some((x) => x.name === selName)) {
        selName = '';
        tree = null;
      }
    } catch (e) {
      err = e.message;
    }
  }
  onMount(load);

  async function act(fn, ok) {
    err = '';
    try {
      await fn();
      if (ok) flash(ok);
      await load();
    } catch (e) {
      err = e.message;
    }
  }
  const enc = encodeURIComponent;
  // Имя устройства Windows (CON, NUL, COM1…) для новой папки — отказ
  // и в gamayund; здесь подсказка до отправки.
  const shareNameErr = $derived(winReservedName(ns.name.trim()) ? t('shares.reservedName', { name: ns.name.trim(), dev: winReservedName(ns.name.trim()) }) : '');
  const create = (e) => {
    e.preventDefault();
    act(async () => {
      await post('/nas/shares', { ...ns, pool: ns.pool || app.pools[0]?.name });
      ns = { name: '', pool: '', comment: '' };
    }, t('shares.created'));
  };

  function level(share, kind, subject) {
    return (share.access || []).find((a) => a.kind === kind && a.subject === subject)?.level || 'none';
  }
  const has = (share, kind, subject) => (share.access || []).some((a) => a.kind === kind && a.subject === subject);
  function openEdit(s) {
    edit = {
      name: s.name,
      rows: [
        ...groups.map((g) => ({ kind: 'group', subject: g.name, level: level(s, 'group', g.name), had: has(s, 'group', g.name) })),
        ...users.map((u) => ({ kind: 'user', subject: u.name, level: level(s, 'user', u.name), had: has(s, 'user', u.name) })),
      ],
    };
  }
  const save = () =>
    act(async () => {
      await put(`/nas/shares/${enc(edit.name)}/access`, { access: edit.rows.filter((r) => r.level !== 'none' || r.had).map(({ kind, subject, level }) => ({ kind, subject, level })) });
      edit = null;
    }, t('shares.accessSaved'));
  const summary = (s) => {
    const a = s.access || [];
    const n = (l) => a.filter((x) => x.level === l).length;
    return t('shares.summary', { rw: n('rw'), read: n('read') });
  };
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
<div class="grid two">
  <section class="card stack">
    <h2>{t('shares.title')} <HelpButton section="shares" /> <span class="tag">{t('shares.tagRoot')}</span></h2>
    <ul class="tree roots">
      {#each shares as s}
        <li>
          <div class:sel={s.name === selName}>
            <button class="link rootbtn" onclick={() => pick(s.name)} aria-pressed={s.name === selName}>
              <Icon name="folder" /><b>{s.name}</b> <span class="muted small sp">{t('shares.poolLine', { pool: s.pool })}{s.comment ? ' · ' + s.comment : ''}</span>
            </button>
            {#if s.read_only}<span class="badge bad">{t('common.readOnly')}</span>{/if}
            <span class="tag">{summary(s)}</span>
          </div>
          <div class="row actions">
            <button onclick={() => openEdit(s)}>{t('shares.access')}</button>
            <button onclick={() => (snaps = s.name)}>{t('shares.snapshots')}</button>
            <Confirm label={t('shares.remove')} hint={t('shares.removeHint')} onconfirm={() => act(() => del(`/nas/shares/${enc(s.name)}`), t('shares.removed'))} />
          </div>
        </li>
      {:else}
        <li class="muted">{t('shares.none')}</li>
      {/each}
    </ul>
    <button onclick={() => act(() => post('/nas/shares/apply'), t('shares.applied'))}>{t('shares.apply')}</button>
  </section>

  <section class="card stack">
    <h2>{selected ? t('shares.inside', { name: selected.name }) : t('shares.nested')} <span class="tag">{t('shares.tagNested')}</span></h2>
    {#if !selected}
      <p class="muted">{t('shares.pickRoot')}</p>
    {:else if !tree}
      <p class="muted">{t('common.loading')}</p>
    {:else if tree.error}
      <p class="error" role="alert">{tree.error}</p>
    {:else}
      {#key tree.share}
        <DirTree share={tree.share} nodes={tree.dirs} root onreload={refresh} />
      {/key}
      {#if tree.partial}<p class="small muted">{t('shares.partial')}</p>{/if}
    {/if}
  </section>

  <section class="card">
    <form onsubmit={create} class="stack">
      <h2>{t('common.newFolder')}</h2>
      <label class="field"><span>{t('common.name')}</span><input bind:value={ns.name} required aria-invalid={!!shareNameErr} aria-describedby={shareNameErr ? 'sharename-err' : undefined} /></label>
      {#if shareNameErr}<p class="error small" id="sharename-err" role="alert">{shareNameErr}</p>{/if}
      <label class="field">
        <span>{t('common.pool')}</span>
        <select bind:value={ns.pool}>
          {#each app.pools as p}<option value={p.name}>{p.name}</option>{/each}
        </select>
      </label>
      <label class="field"><span>{t('shares.description')}</span><input bind:value={ns.comment} /></label>
      {#if !app.pools.length}<p class="small muted">{t('common.createPoolFirst')}</p>{/if}
      <button class="primary" disabled={!app.pools.length || !!shareNameErr}>{t('shares.createFolder')}</button>
    </form>
  </section>
</div>

{#if edit}
  <Drawer title={t('shares.accessTitle', { name: edit.name })} onclose={() => (edit = null)}>
    <div class="stack">
      {#each edit.rows as r}
        <div class="set">
          <span class="sp"><b>{r.subject}</b><small>{r.kind === 'group' ? t('shares.group') : t('shares.user')}</small></span>
          <Seg options={LEVELS} value={r.level} label={t('shares.accessAria', { name: r.subject })} onchange={(v) => (r.level = v)} />
        </div>
      {:else}
        <p class="muted">{t('shares.noSubjects')}</p>
      {/each}
      <button class="primary" onclick={save}>{t('common.save')}</button>
    </div>
  </Drawer>
{/if}

{#if snaps}
  <Drawer title={t('shares.snapshotsTitle', { name: snaps })} onclose={() => (snaps = '')}>
    {#key snaps}<Snapshots share={snaps} />{/key}
  </Drawer>
{/if}
