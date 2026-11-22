// справка — разбор, оглавление, поиск, Markdown, ссылки help:id и «?».
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { parseSection, buildIndex, sectionIn, sectionsIn, searchIn, helpLinks, inline, blocks, PLANNED } from './parse.js';
import { poolTopic } from './topic.js';

const dir = new URL('.', import.meta.url).pathname;
const src = join(dir, '..');
function readTexts() {
  const files = {};
  for (const l of readdirSync(dir)) {
    const d = join(dir, l);
    if (!statSync(d).isDirectory()) continue;
    for (const f of readdirSync(d)) if (f.endsWith('.md')) files[`./${l}/${f}`] = readFileSync(join(d, f), 'utf8');
  }
  return files;
}
const IDX = buildIndex(readTexts());

test('parseSection: заголовок из первой «# », без него — id', () => {
  assert.deepEqual(parseSection('a', '# Заголовок\r\nтекст'), { id: 'a', title: 'Заголовок', body: 'текст' });
  assert.equal(parseSection('a', 'просто текст').title, 'a');
});

test('buildIndex: откат на ru, порядок оглавления, поиск', () => {
  const idx = buildIndex({ './ru/storage.pools.md': '# Пулы\nдиски', './ru/overview.md': '# Обзор\nвсё', './ru/storage.md': '# Хранилище\nx', './en/overview.md': '# Overview\nall', 'bad.md': 'x' });
  assert.equal(sectionIn(idx, 'overview', 'en').title, 'Overview');
  assert.equal(sectionIn(idx, 'storage', 'en').title, 'Хранилище', 'нет en — ru');
  assert.equal(sectionIn(idx, 'nope', 'ru'), null);
  assert.deepEqual(sectionsIn(idx, 'ru').map((s) => s.id), ['overview', 'storage', 'storage.pools']);
  assert.deepEqual(searchIn(idx, 'ДИСКИ', 'ru').map((s) => s.id), ['storage.pools']);
  assert.deepEqual(searchIn(idx, '  ', 'ru'), []);
});

test('inline: жирный, код, ссылки; не http — текст', () => {
  assert.deepEqual(inline('a **b** `c` [d](help:x.y) [e](https://e.org) [f](javascript:x) <i>'), [
    { t: 'text', v: 'a ' }, { t: 'b', v: 'b' }, { t: 'text', v: ' ' }, { t: 'code', v: 'c' }, { t: 'text', v: ' ' },
    { t: 'help', v: 'd', href: 'x.y' }, { t: 'text', v: ' ' }, { t: 'link', v: 'e', href: 'https://e.org' }, { t: 'text', v: ' ' },
    { t: 'text', v: 'f' }, { t: 'text', v: ' <i>' },
  ]);
  assert.deepEqual(helpLinks('[a](help:x) и [b](help:y.z)'), ['x', 'y.z']);
});

test('blocks: заголовки, списки с переносом, таблица, код, details', () => {
  const b = blocks('## Раз\n\nабзац\nпродолжение\n- п1\n  дальше\n- п2\n1. н1\n| A | B |\n|---|---|\n| 1 | 2 |\n```\nкод\n```\n:::details Тех\n### Под\nx\n:::\n:::\nхвост');
  assert.deepEqual(b.map((x) => x.t), ['h3', 'p', 'ul', 'ol', 'table', 'pre', 'details', 'p', 'p']);
  assert.equal(b[1].v[0].v, 'абзац продолжение');
  assert.equal(b[2].items.length, 2);
  assert.equal(b[2].items[0][0].v, 'п1 дальше');
  assert.equal(b[4].head.length, 2);
  assert.equal(b[4].body.length, 1, 'строка-разделитель отброшена');
  assert.equal(b[5].v, 'код');
  assert.equal(b[6].summary, 'Тех');
  assert.deepEqual(b[6].children.map((x) => x.t), ['h4', 'p']);
});

test('тексты: id из списка help-style, заголовок, ссылки help: на написанные разделы', () => {
  const ru = IDX.ru || {};
  assert.ok(Object.keys(ru).length >= 3, 'разделы ru есть');
  for (const [l, secs] of Object.entries(IDX)) {
    for (const [id, s] of Object.entries(secs)) {
      assert.ok(PLANNED.includes(id), `${l}/${id}: id нет в PLANNED`);
      assert.notEqual(s.title, id, `${l}/${id}: нет заголовка «# »`);
      assert.doesNotMatch(s.body, /не проверено/i, `${l}/${id}: пометка «не проверено» в готовом разделе`);
      for (const to of helpLinks(s.body)) assert.ok(ru[to], `${l}/${id}: ссылка help:${to} на ненаписанный раздел`);
    }
  }
});

function svelteFiles(d, out = []) {
  for (const f of readdirSync(d)) {
    const p = join(d, f);
    if (statSync(p).isDirectory()) svelteFiles(p, out);
    else if (f.endsWith('.svelte')) out.push(p);
  }
  return out;
}

test('«?»: section у HelpButton — написанный раздел; динамический — из poolTopic', () => {
  let n = 0;
  for (const f of svelteFiles(src)) {
    const s = readFileSync(f, 'utf8');
    for (const m of s.matchAll(/<HelpButton\s+section=(?:"([^"]+)"|\{([^}]+)\})/g)) {
      n++;
      if (m[1]) assert.ok(IDX.ru?.[m[1]], `${f}: раздел ${m[1]} не написан`);
      else assert.match(m[2], /^(topic|help|poolTopic\(\w+\))$/, `${f}: неизвестный источник раздела ${m[2]}`);
    }
  }
  assert.ok(n >= 4, 'кнопки «?» есть');
});

test('«?» в заголовке окна: у каждого раздела меню есть справка ru и en', async () => {
  const src = readFileSync(new URL('../shell/sections.js', import.meta.url), 'utf8');
  const ids = [...src.matchAll(/sec\('([a-z]+)'/g)].map((m) => m[1]).filter((id) => !['help', 'backup'].includes(id));
  assert.ok(ids.length >= 8);
  for (const id of ids) {
    assert.ok(IDX.ru?.[id], `нет справки ru: ${id}`);
    assert.ok(IDX.en?.[id], `нет справки en: ${id}`);
  }
});

test('справка без {@html}', () => {
  for (const f of svelteFiles(dir)) assert.doesNotMatch(readFileSync(f, 'utf8'), /\{@html/, f);
});

test('poolTopic: раздел по полям статуса, всегда из списка', () => {
  const P = (status, protection = {}, extra = {}) => ({ status, protection, ...extra });
  assert.equal(poolTopic(P({ health: 'ok', issues: [] })), null);
  assert.equal(poolTopic(P({ health: 'degraded' }, { write_blocked: true })), 'storage.pools.incomplete');
  assert.equal(poolTopic(P({ health: 'ok' }, { override: { actor: 'a' } })), 'storage.pools.readonly');
  assert.equal(poolTopic(P({ health: 'ok' }, { read_only: true })), 'storage.pools.readonly');
  assert.equal(poolTopic(P({ health: 'warning', emergency_read_only: true })), 'storage.pools.readonly');
  assert.equal(poolTopic(P({ health: 'unknown' })), 'storage.pools.unknown');
  assert.equal(poolTopic(P({ health: 'failed' })), 'storage.pools.failed');
  assert.equal(poolTopic(P({ health: 'missing' })), 'storage.pools.missing');
  assert.equal(poolTopic(P({ health: 'ok', integrity_errors: 3 })), 'storage.pools.integrity');
  assert.equal(poolTopic(P({ health: 'ok' }, {}, { pending: { disk: {} } })), 'storage.pools.pending');
  assert.equal(poolTopic(P({ health: 'ok' }, {}, { pending: { disk: {} }, busy: 'x' })), null);
  assert.equal(poolTopic(P(undefined)), null);
  const all = [poolTopic(P({ health: 'degraded' })), poolTopic(P({ health: 'unknown' })), poolTopic(P({ health: 'failed' })), poolTopic(P({ health: 'missing' })), poolTopic(P({ health: 'ok', integrity_errors: 1 }))];
  for (const id of all) assert.ok(PLANNED.includes(id), id);
});
