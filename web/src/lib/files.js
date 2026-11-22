// Веб-файловый менеджер — чистые функции (без DOM), проверяются тестами.
import { dirNameError } from './names.js';
import { t } from './i18n/index.js';

const enc = encodeURIComponent;

// Путь внутри папки: '' — корень; части через '/'.
export const join = (dir, name) => (dir ? dir + '/' + name : name);
export const parent = (p) => (p.includes('/') ? p.slice(0, p.lastIndexOf('/')) : '');
export const baseName = (p) => p.slice(p.lastIndexOf('/') + 1);

// Хлебные крошки: [{label, path}], первая — сама папка.
export function crumbs(rootLabel, p) {
  const out = [{ label: rootLabel, path: '' }];
  if (!p) return out;
  let cur = '';
  for (const part of p.split('/')) {
    cur = join(cur, part);
    out.push({ label: part, path: cur });
  }
  return out;
}

// Подпись корня: домашняя папка — «Личная папка».
export const rootLabel = (r) => (r.home ? t('files.rootHome') : r.name);

// Сортировка: папки первыми, затем по ключу; имя — без учёта регистра, по-русски.
const coll = new Intl.Collator('ru', { numeric: true, sensitivity: 'base' });
export function sortEntries(list, key = 'name', desc = false) {
  const rank = (e) => (e.type === 'dir' ? 0 : 1);
  const cmp = {
    name: (a, b) => coll.compare(a.name, b.name),
    size: (a, b) => (a.size || 0) - (b.size || 0) || coll.compare(a.name, b.name),
    mtime: (a, b) => new Date(a.mtime) - new Date(b.mtime) || coll.compare(a.name, b.name),
  }[key];
  return [...list].sort((a, b) => rank(a) - rank(b) || (desc ? -cmp(a, b) : cmp(a, b)));
}

// Адреса API.
export const listUrl = (root, p) => `/files/${enc(root)}/list?path=${enc(p)}`;
export const downloadUrl = (root, p) => `/api/v1/files/${enc(root)}/download?path=${enc(p)}`;
export const uploadUrl = (root, dir, name, onConflict) =>
  `/api/v1/files/${enc(root)}/upload?path=${enc(dir)}&name=${enc(name)}&on_conflict=${onConflict}`;

// Имя нового файла/папки: правила Windows (как папки) и не служебное.
export function fileNameError(name) {
  const e = dirNameError(name, 'file');
  if (e) return e;
  if (name.startsWith('.gamayun-up-')) return t('files.reserved');
  return '';
}

// Куда можно скопировать/перенести выбранное: запрет в себя и в своих потомков
// той же папки. Возвращает текст ошибки или ''.
export function transferError(srcRoot, paths, dstRoot, dstPath) {
  if (!paths.length) return t('files.nothingSelected');
  if (srcRoot !== dstRoot) return '';
  for (const p of paths) {
    if (dstPath === p || dstPath.startsWith(p + '/')) return t('files.intoItself', { name: baseName(p) });
  }
  return '';
}

// Перенос в тот же каталог бессмыслен.
export const sameDir = (srcRoot, paths, dstRoot, dstPath) => srcRoot === dstRoot && paths.every((p) => parent(p) === dstPath);

// Итог задачи для сообщения.
export function transferSummary(op, task) {
  const verb = ['copy', 'move', 'delete'].includes(op) ? t('files.summary.' + op) : t('files.summary.done');
  const r = task.result || {};
  const done = (r.done || []).length;
  if (task.state === 'succeeded') return t('files.summary.ok', { verb, done });
  // Отмена в менеджере задач — failed с кодом cancelled.
  if (task.error && task.error.code === 'cancelled') return done ? t('files.summary.cancelledDone', { done }) : t('files.summary.cancelled');
  return (task.error && task.error.message) || t('files.summary.failed');
}

// Перетаскиваемые файлы: только файлы (папки браузер отдаёт без содержимого).
export function droppedFiles(dt) {
  const out = [];
  for (const it of dt.items ? [...dt.items] : []) {
    if (it.kind !== 'file') continue;
    const en = it.webkitGetAsEntry ? it.webkitGetAsEntry() : null;
    if (en && en.isDirectory) continue;
    const f = it.getAsFile();
    if (f) out.push(f);
  }
  if (!out.length && dt.files) out.push(...dt.files);
  return out;
}
