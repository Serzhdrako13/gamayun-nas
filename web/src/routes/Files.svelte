<script>
  // Веб-файловый менеджер. Права — как в SMB: в папке «только чтение»
  // кнопки изменения скрыты (gamayund проверяет повторно). Загрузка — XHR с
  // прогрессом (тело — файл, Content-Length по нему); копирование, перенос и
  // удаление — задача files.transfer с прогрессом и отменой.
  import { onMount } from 'svelte';
  import { get, post, del, csrfToken, ApiError } from '../lib/api.js';
  import { flash } from '../lib/state.svelte.js';
  import { bytes } from '../lib/format.js';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';
  import { t, locale } from '../lib/i18n/index.js';
  import {
    join, parent, baseName, crumbs, rootLabel, sortEntries, listUrl, downloadUrl, uploadUrl,
    fileNameError, transferError, sameDir, transferSummary, droppedFiles,
  } from '../lib/files.js';

  let roots = $state([]);
  let root = $state('');
  let path = $state('');
  let dir = $state(null); // {level, entries, truncated}
  let err = $state('');
  let sel = $state({}); // имя → true
  let sortKey = $state('name');
  let sortDesc = $state(false);
  let form = $state(null); // {mode: 'mkdir'|'rename', value, path?}
  let xfer = $state(null); // {op, toRoot, toPath}
  let rename = $state(true); // при совпадении имени — сохранить с номером
  let uploads = $state([]); // [{name, loaded, total, state, error}]
  let task = $state(null); // {id, op, progress, state}
  let drag = $state(false);
  let fileInput;

  const cur = $derived(roots.find((r) => r.name === root));
  const rw = $derived(dir?.level === 'rw');
  const entries = $derived(dir ? sortEntries(dir.entries, sortKey, sortDesc) : []);
  const chosen = $derived(entries.filter((e) => sel[e.name]));
  const chosenPaths = $derived(chosen.map((e) => join(path, e.name)));
  const nameErr = $derived(form && form.value.trim() ? fileNameError(form.value.trim()) : '');
  const xferErr = $derived(xfer ? transferError(root, chosenPaths, xfer.toRoot, xfer.toPath.trim()) : '');
  const time = (v) => new Date(v).toLocaleString(locale());
  const ICON = { dir: 'folder', file: 'log', link: 'log', other: 'log' };
  const opLabel = (op) => t('files.op.' + op);

  async function loadRoots() {
    try {
      roots = (await get('/files')).roots || [];
      if (!roots.some((r) => r.name === root)) root = roots[0]?.name || '';
      if (root) await open('');
    } catch (e) {
      err = e.message;
    }
  }
  async function open(p) {
    err = '';
    try {
      const r = await get(listUrl(root, p));
      path = r.path;
      dir = r;
      sel = {};
      form = null;
    } catch (e) {
      err = e.message;
    }
  }
  const reload = () => open(path);
  function pickRoot(name) {
    root = name;
    path = '';
    dir = null;
    open('');
  }
  function sortBy(k) {
    if (sortKey === k) sortDesc = !sortDesc;
    else { sortKey = k; sortDesc = false; }
  }
  const allSel = $derived(entries.length > 0 && chosen.length === entries.length);
  function toggleAll() {
    const v = !allSel;
    sel = Object.fromEntries(entries.map((e) => [e.name, v]));
  }

  async function submitForm(e) {
    e.preventDefault();
    const name = form.value.trim();
    if (!name || nameErr) return;
    try {
      if (form.mode === 'mkdir') {
        await post(`/files/${encodeURIComponent(root)}/mkdir`, { path, name });
        flash(t('files.created', { name }));
      } else {
        await post(`/files/${encodeURIComponent(root)}/rename`, { path: form.path, name });
        flash(t('files.renamed', { name }));
      }
      await reload();
    } catch (e2) {
      err = e2.status === 409 ? t('files.exists', { name }) : e2.message;
    }
  }

  // Загрузка: по одному файлу, последовательно.
  function uploadOne(item, file) {
    return new Promise((resolve) => {
      const x = new XMLHttpRequest();
      x.open('POST', uploadUrl(root, path, file.name, rename ? 'rename' : 'error'));
      x.setRequestHeader('X-CSRF-Token', csrfToken());
      x.setRequestHeader('Content-Type', 'application/octet-stream');
      x.upload.onprogress = (ev) => { item.loaded = ev.loaded; item.total = ev.total || file.size; };
      x.onload = () => {
        let msg = '';
        try { msg = JSON.parse(x.responseText)?.error?.message || ''; } catch { /* не JSON */ }
        if (x.status === 201) item.state = 'done';
        else {
          item.state = 'error';
          item.error = x.status === 409 ? t('files.upload.nameTaken') : x.status === 507 ? t('files.upload.noSpace') : msg || t('files.upload.failed', { code: x.status });
        }
        resolve();
      };
      x.onerror = () => { item.state = 'error'; item.error = t('files.upload.noConnection'); resolve(); };
      item.abort = () => x.abort();
      x.onabort = () => { item.state = 'error'; item.error = t('files.upload.cancelled'); resolve(); };
      x.send(file);
    });
  }
  async function upload(list) {
    const files = [...list];
    if (!files.length || !rw) return;
    const bad = files.find((f) => fileNameError(f.name));
    if (bad) { err = t('files.upload.badName', { name: bad.name, error: fileNameError(bad.name) }); return; }
    const items = files.map((f) => ({ name: f.name, loaded: 0, total: f.size, state: 'wait', error: '' }));
    uploads = [...uploads.filter((u) => u.state === 'wait' || u.state === 'run'), ...items];
    const proxied = uploads.slice(-items.length);
    for (let i = 0; i < files.length; i++) {
      proxied[i].state = 'run';
      await uploadOne(proxied[i], files[i]);
    }
    await reload();
    const failed = proxied.filter((u) => u.state === 'error').length;
    flash(failed ? t('files.upload.partial', { ok: files.length - failed, total: files.length }) : t('files.upload.done', { count: files.length }));
  }
  function onDrop(e) {
    e.preventDefault();
    drag = false;
    if (rw) upload(droppedFiles(e.dataTransfer));
  }

  function download() {
    for (const e of chosen) {
      if (e.type !== 'file' && e.type !== 'dir') continue;
      const a = document.createElement('a');
      a.href = downloadUrl(root, join(path, e.name));
      a.download = '';
      document.body.append(a);
      a.click();
      a.remove();
    }
  }

  async function startTask(req) {
    try {
      const r = await post('/files/transfer', req);
      task = { id: r.id, op: req.op, progress: 0, state: 'queued' };
      xfer = null;
      poll();
    } catch (e) {
      err = e.message;
    }
  }
  async function poll() {
    while (task && (task.state === 'queued' || task.state === 'running')) {
      await new Promise((r) => setTimeout(r, 700));
      try {
        const r = await get(`/tasks/${task.id}`);
        task = { ...task, state: r.state, progress: r.progress, result: r.result, error: r.error };
      } catch (e) {
        err = e.message;
        break;
      }
    }
    if (task) {
      const msg = transferSummary(task.op, task);
      if (task.state === 'succeeded') flash(msg);
      else err = msg;
      task = null;
      reload();
    }
  }
  async function cancelTask() {
    try { await del(`/files/tasks/${task.id}`); } catch (e) { if (!(e instanceof ApiError && e.status === 409)) err = e.message; }
  }
  function removeChosen() {
    startTask({ op: 'delete', root, paths: chosenPaths });
  }
  function submitXfer(e) {
    e.preventDefault();
    if (xferErr) return;
    const to = xfer.toPath.trim().replace(/^\/+|\/+$/g, '');
    if (xfer.op === 'move' && sameDir(root, chosenPaths, xfer.toRoot, to)) { xfer = null; return; }
    startTask({ op: xfer.op, root, paths: chosenPaths, to_root: xfer.toRoot, to_path: to, on_conflict: rename ? 'rename' : 'error' });
  }
  const writable = $derived(roots.filter((r) => r.level === 'rw'));

  onMount(loadRoots);
</script>

{#if err}<p class="error" role="alert">{err}</p>{/if}
{#if !roots.length && !err}
  <section class="card"><p class="muted">{t('files.noRoots')}</p></section>
{:else if roots.length}
  <section class="card stack fm" class:drag ondragover={(e) => { if (rw) { e.preventDefault(); drag = true; } }} ondragleave={() => (drag = false)} ondrop={onDrop} aria-label={t('files.aria')}>
    <div class="row between">
      <div class="row">
        <label class="row small">{t('files.folder')}
          <select value={root} onchange={(e) => pickRoot(e.currentTarget.value)}>
            {#each roots as r}<option value={r.name}>{rootLabel(r)}{r.level === 'read' ? t('files.readSuffix') : ''}</option>{/each}
          </select>
        </label>
        <nav class="crumbs small" aria-label={t('files.pathAria')}>
          {#each crumbs(cur ? rootLabel(cur) : root, path) as c, i}
            {#if i}<span class="muted">/</span>{/if}
            <button class="link" onclick={() => open(c.path)} aria-current={c.path === path ? 'page' : undefined}>{c.label}</button>
          {/each}
        </nav>
      </div>
      <button onclick={reload}>{t('common.refresh')}</button>
    </div>

    <div class="row tools">
      {#if rw}
        <button onclick={() => fileInput.click()}><Icon name="folder" /> {t('files.upload.button')}</button>
        <input type="file" multiple hidden bind:this={fileInput} onchange={(e) => { upload(e.currentTarget.files); e.currentTarget.value = ''; }} />
        <button onclick={() => (form = { mode: 'mkdir', value: '' })}>{t('common.newFolder')}</button>
      {/if}
      <button disabled={!chosen.length} onclick={download} title={t('files.downloadTitle')}>{t('files.download')}</button>
      {#if rw}
        <button disabled={chosen.length !== 1} onclick={() => (form = { mode: 'rename', value: chosen[0].name, path: chosenPaths[0] })}>{t('common.rename')}</button>
      {/if}
      <button disabled={!chosen.length || !writable.length} onclick={() => (xfer = { op: 'copy', toRoot: writable.some((r) => r.name === root) ? root : writable[0].name, toPath: path })}>{t('files.copyBtn')}</button>
      {#if rw}
        <button disabled={!chosen.length} onclick={() => (xfer = { op: 'move', toRoot: root, toPath: path })}>{t('files.moveBtn')}</button>
        <Confirm label={chosen.length > 1 ? t('files.deleteN', { count: chosen.length }) : t('common.delete')} disabled={!chosen.length} onconfirm={removeChosen} hint={t('files.deleteHint')} />
      {/if}
      {#if rw}<label class="row small muted" title={t('files.numberedTitle')}><input type="checkbox" bind:checked={rename} /> {t('files.numbered')}</label>{/if}
    </div>

    {#if form}
      <form class="row" onsubmit={submitForm}>
        <label>{form.mode === 'mkdir' ? t('files.newFolderName') : t('files.newName')}
          <input bind:value={form.value} required maxlength="255" aria-invalid={!!nameErr} />
        </label>
        <button class="primary" disabled={!!nameErr}>{form.mode === 'mkdir' ? t('common.create') : t('common.rename')}</button>
        <button type="button" onclick={() => (form = null)}>{t('common.cancel')}</button>
        {#if nameErr}<span class="error small">{nameErr}</span>{/if}
      </form>
    {/if}

    {#if xfer}
      <form class="row" onsubmit={submitXfer}>
        <strong>{t('files.xferTo', { op: opLabel(xfer.op), count: chosen.length })}</strong>
        <select bind:value={xfer.toRoot} aria-label={t('files.destAria')}>
          {#each writable as r}<option value={r.name}>{rootLabel(r)}</option>{/each}
        </select>
        <label class="row small">{t('files.folderLower')} <input bind:value={xfer.toPath} placeholder={t('files.emptyRoot')} /></label>
        <button class="primary" disabled={!!xferErr}>{opLabel(xfer.op)}</button>
        <button type="button" onclick={() => (xfer = null)}>{t('common.cancel')}</button>
        {#if xferErr}<span class="error small">{xferErr}</span>{/if}
      </form>
    {/if}

    {#if task}
      <div class="row" role="status">
        <span>{t('files.progress.' + task.op)}… {task.progress}%</span>
        <progress max="100" value={task.progress}></progress>
        <button onclick={cancelTask}>{t('common.abort')}</button>
      </div>
    {/if}

    {#if uploads.length}
      <div class="stack small">
        {#each uploads as u}
          <div class="row">
            <span class="name">{u.name}</span>
            {#if u.state === 'run'}<progress max={u.total || 1} value={u.loaded}></progress><button onclick={() => u.abort?.()}>{t('common.abort')}</button>
            {:else if u.state === 'done'}<span class="badge ok">{t('files.upload.uploaded')}</span>
            {:else if u.state === 'error'}<span class="badge bad">{u.error}</span>
            {:else}<span class="muted">{t('files.upload.queued')}</span>{/if}
          </div>
        {/each}
        {#if uploads.every((u) => u.state === 'done' || u.state === 'error')}<button class="link" onclick={() => (uploads = [])}>{t('files.upload.hide')}</button>{/if}
      </div>
    {/if}

    {#if dir}
      {#if !entries.length}
        <p class="muted">{t('files.empty')}{#if rw} {t('files.emptyDrop')}{/if}</p>
      {:else}
        <table class="responsive">
          <thead><tr>
            <th><input type="checkbox" checked={allSel} onchange={toggleAll} aria-label={t('files.selectAll')} /></th>
            <th><button class="link" onclick={() => sortBy('name')}>{t('common.name')}{sortKey === 'name' ? (sortDesc ? ' ↓' : ' ↑') : ''}</button></th>
            <th><button class="link" onclick={() => sortBy('size')}>{t('files.size')}{sortKey === 'size' ? (sortDesc ? ' ↓' : ' ↑') : ''}</button></th>
            <th><button class="link" onclick={() => sortBy('mtime')}>{t('files.modified')}{sortKey === 'mtime' ? (sortDesc ? ' ↓' : ' ↑') : ''}</button></th>
          </tr></thead>
          <tbody>
            {#each entries as e (e.name)}
              <tr class:on={sel[e.name]}>
                <td><input type="checkbox" bind:checked={sel[e.name]} aria-label={t('files.select', { name: e.name })} /></td>
                <td data-label={t('common.name')}>
                  {#if e.type === 'dir'}
                    <button class="link name" onclick={() => open(join(path, e.name))}><Icon name="folder" /> {e.name}</button>
                  {:else if e.type === 'file'}
                    <a class="name" href={downloadUrl(root, join(path, e.name))} download><Icon name={ICON[e.type]} /> {e.name}</a>
                  {:else}
                    <span class="name muted" title={e.type === 'link' ? t('files.linkTitle') : t('files.special')}><Icon name="log" /> {e.name}{e.type === 'link' ? ' →' : ''}</span>
                  {/if}
                </td>
                <td data-label={t('files.size')}>{e.type === 'file' ? bytes(e.size) : ''}</td>
                <td data-label={t('files.modified')} class="small muted">{time(e.mtime)}</td>
              </tr>
            {/each}
          </tbody>
        </table>
        {#if dir.truncated}<p class="muted small">{t('files.truncated')}</p>{/if}
      {/if}
      {#if path}<button class="link small" onclick={() => open(parent(path))}>← {parent(path) ? baseName(parent(path)) : (cur ? rootLabel(cur) : root)}</button>{/if}
    {/if}
  </section>
{/if}

<style>
  .fm.drag { outline: 2px dashed var(--accent); outline-offset: -6px; }
  .crumbs { display: flex; gap: 4px; align-items: center; flex-wrap: wrap; }
  .crumbs [aria-current] { font-weight: 600; }
  .tools { gap: 8px; }
  .name { display: inline-flex; gap: 6px; align-items: center; word-break: break-all; }
  tr.on { background: var(--surface2); }
  progress { width: 160px; }
  button.link { background: none; border: 0; padding: 0; color: inherit; cursor: pointer; text-decoration: underline dotted; }
  th button.link { text-decoration: none; font-weight: inherit; }
</style>
