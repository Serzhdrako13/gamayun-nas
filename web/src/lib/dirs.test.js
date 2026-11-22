import { test } from 'node:test';
import assert from 'node:assert/strict';
import { moveTargets, moveError } from './dirs.js';

const tree = [
  { name: 'a', path: 'a', children: [
    { name: 'b', path: 'a/b', children: [{ name: 'c', path: 'a/b/c', children: null }] },
    { name: 'd', path: 'a/d', children: [] },
  ] },
  { name: 'ab', path: 'ab', children: [] },
  { name: 'Фото', path: 'Фото', children: [] },
];

test('цели переноса — без себя, потомков и текущего родителя', () => {
  const r = moveTargets(tree, 'a/b');
  assert.deepEqual(r.targets, ['', 'a/d', 'ab', 'Фото']);
  assert.equal(r.incomplete, false);
  const top = moveTargets(tree, 'ab');
  assert.ok(!top.targets.includes(''), 'корень — текущий родитель');
  assert.ok(top.targets.includes('a/b/c'));
  assert.equal(top.incomplete, true, 'a/b/c не раскрыт');
  assert.equal(moveTargets(tree, 'Фото', true).incomplete, true);
  assert.ok(!moveTargets(tree, 'a').targets.some((p) => p === 'a' || p.startsWith('a/')));
  assert.ok(moveTargets(tree, 'a').targets.includes('ab'), 'ab — не потомок a');
});

test('проверка цели', () => {
  assert.equal(moveError('a/b', 'ab'), '');
  assert.equal(moveError('a/b', ''), '');
  assert.match(moveError('a', 'a'), /саму себя/);
  assert.match(moveError('a', 'a/x'), /подкаталог/);
  assert.equal(moveError('a', 'ab'), '');
  assert.match(moveError('a/b', 'a'), /уже в этом/);
  assert.match(moveError('a', '/etc'), /относительно/);
  assert.match(moveError('a', 'x/../y'), /относительно/);
  assert.match(moveError('a', 'x/'), /относительно/);
});
