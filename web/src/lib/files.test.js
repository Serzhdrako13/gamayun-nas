import { test } from 'node:test';
import assert from 'node:assert/strict';
import { join, parent, baseName, crumbs, sortEntries, listUrl, downloadUrl, uploadUrl, fileNameError, transferError, sameDir, transferSummary, rootLabel } from './files.js';

test('пути и крошки', () => {
  assert.equal(join('', 'a'), 'a');
  assert.equal(join('a', 'b'), 'a/b');
  assert.equal(parent('a/b/c'), 'a/b');
  assert.equal(parent('a'), '');
  assert.equal(baseName('a/b/c.txt'), 'c.txt');
  assert.deepEqual(crumbs('docs', 'a/b'), [{ label: 'docs', path: '' }, { label: 'a', path: 'a' }, { label: 'b', path: 'a/b' }]);
  assert.deepEqual(crumbs('docs', ''), [{ label: 'docs', path: '' }]);
  assert.equal(rootLabel({ name: 'homes', home: true }), 'Личная папка');
});

test('сортировка — папки первыми', () => {
  const l = [
    { name: 'б.txt', type: 'file', size: 5, mtime: '2026-01-02T00:00:00Z' },
    { name: 'Я', type: 'dir', size: 0, mtime: '2026-01-01T00:00:00Z' },
    { name: 'а10.txt', type: 'file', size: 50, mtime: '2026-01-03T00:00:00Z' },
    { name: 'а2.txt', type: 'file', size: 1, mtime: '2026-01-01T00:00:00Z' },
  ];
  assert.deepEqual(sortEntries(l).map((e) => e.name), ['Я', 'а2.txt', 'а10.txt', 'б.txt']);
  assert.deepEqual(sortEntries(l, 'size', true).map((e) => e.name), ['Я', 'а10.txt', 'б.txt', 'а2.txt']);
  assert.deepEqual(sortEntries(l, 'mtime').map((e) => e.name), ['Я', 'а2.txt', 'б.txt', 'а10.txt']);
});

test('адреса API экранируются', () => {
  assert.equal(listUrl('docs', 'a b/в'), '/files/docs/list?path=a%20b%2F%D0%B2');
  assert.equal(downloadUrl('docs', 'x&y'), '/api/v1/files/docs/download?path=x%26y');
  assert.equal(uploadUrl('docs', '', 'a#b.txt', 'rename'), '/api/v1/files/docs/upload?path=&name=a%23b.txt&on_conflict=rename');
});

test('имена и цели переноса', () => {
  assert.equal(fileNameError('отчёт.txt'), '');
  assert.match(fileNameError('a:b'), /символ «:»/);
  assert.match(fileNameError('CON.txt'), /зарезервированное/);
  assert.equal(fileNameError('.gamayun-up-x'), 'имя зарезервировано');
  assert.match(transferError('docs', ['a'], 'docs', 'a/b'), /в саму себя/);
  assert.match(transferError('docs', ['a'], 'docs', 'a'), /в саму себя/);
  assert.equal(transferError('docs', ['a'], 'docs', 'ab'), '');
  assert.equal(transferError('docs', ['a'], 'media', 'a'), '');
  assert.ok(sameDir('docs', ['x/a', 'x/b'], 'docs', 'x'));
  assert.ok(!sameDir('docs', ['x/a'], 'media', 'x'));
});

test('итог задачи', () => {
  assert.equal(transferSummary('copy', { state: 'succeeded', result: { done: ['a', 'b'] } }), 'Скопировано: 2');
  assert.equal(transferSummary('move', { state: 'failed', error: { code: 'cancelled', message: 'отменена пользователем' }, result: { done: ['a'] } }), 'Отменено; выполнено: 1');
  assert.equal(transferSummary('delete', { state: 'failed', error: { message: 'не выполнено 1 из 1' } }), 'не выполнено 1 из 1');
});
