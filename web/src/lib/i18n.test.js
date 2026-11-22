// i18n — t(), множественное число, отсутствие ключа, полнота словарей.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';
import { DICTS, t, tIn, pluralForm, detectLang, normLang, flatKeys, bindLangState, lang, locale } from './i18n/index.js';

test('t: строка, подстановка, язык по умолчанию ru', () => {
  assert.equal(lang(), 'ru');
  assert.equal(t('common.cancel'), 'Отмена');
  assert.equal(tIn('en', 'common.cancel'), 'Cancel');
  DICTS.ru.__t = { hi: 'Привет, {name}! {name}, {nope}' };
  assert.equal(t('__t.hi', { name: 'Ада' }), 'Привет, Ада! Ада, {nope}');
  delete DICTS.ru.__t;
});

test('t: смена языка через подключённое хранилище', () => {
  const s = { lang: 'en' };
  bindLangState(s);
  try {
    assert.equal(t('common.save'), 'Save');
    assert.equal(locale(), 'en-US');
    s.lang = 'ru';
    assert.equal(t('common.save'), 'Сохранить');
    assert.equal(locale(), 'ru-RU');
  } finally {
    bindLangState({ lang: 'ru' });
  }
});

test('pluralForm: ru 1 / 2–4 / 5+ и исключения 11–14', () => {
  const cases = { 0: 'many', 1: 'one', 2: 'few', 4: 'few', 5: 'many', 11: 'many', 12: 'many', 14: 'many', 21: 'one', 22: 'few', 25: 'many', 101: 'one', 111: 'many', 112: 'many', 122: 'few' };
  for (const [n, f] of Object.entries(cases)) assert.equal(pluralForm('ru', Number(n)), f, n);
  assert.equal(pluralForm('ru', 1.5), 'few');
  assert.equal(pluralForm('en', 1), 'one');
  for (const n of [0, 2, 5, 21, 1.5]) assert.equal(pluralForm('en', n), 'other', String(n));
});

test('t: множественное число по count', () => {
  DICTS.ru.__p = { disks: { one: '{count} диск', few: '{count} диска', many: '{count} дисков' } };
  DICTS.en.__p = { disks: { one: '{count} disk', other: '{count} disks' } };
  try {
    assert.deepEqual([1, 3, 5, 21].map((n) => t('__p.disks', { count: n })), ['1 диск', '3 диска', '5 дисков', '21 диск']);
    assert.deepEqual([1, 3].map((n) => tIn('en', '__p.disks', { count: n })), ['1 disk', '3 disks']);
  } finally {
    delete DICTS.ru.__p;
    delete DICTS.en.__p;
  }
});

test('t: нет ключа — сам ключ и одно предупреждение', () => {
  const orig = console.warn;
  const got = [];
  console.warn = (m) => got.push(m);
  try {
    assert.equal(t('nope.missing'), 'nope.missing');
    assert.equal(t('nope.missing'), 'nope.missing');
    assert.equal(t('common'), 'common', 'раздел — не строка');
  } finally {
    console.warn = orig;
  }
  assert.equal(got.length, 2);
  assert.match(got[0], /nope\.missing/);
});

test('язык браузера: ru/en, иначе ru', () => {
  assert.equal(detectLang(['en-US', 'ru']), 'en');
  assert.equal(detectLang(['ru-RU']), 'ru');
  assert.equal(detectLang(['de-DE', 'en']), 'ru');
  assert.equal(detectLang([]), 'ru');
  assert.equal(detectLang(undefined), 'ru');
  assert.equal(normLang('EN'), 'en');
  assert.equal(normLang('eng'), '');
});

test('словари: все ключи ru есть в en и наоборот', () => {
  const ru = new Set(flatKeys(DICTS.ru));
  const en = new Set(flatKeys(DICTS.en));
  assert.ok(ru.size > 100, 'словарь ru пуст?');
  assert.deepEqual([...ru].filter((k) => !en.has(k)), [], 'нет в en');
  assert.deepEqual([...en].filter((k) => !ru.has(k)), [], 'нет в ru');
});

test('словари: формы множественного числа и подстановки совпадают', () => {
  const bad = [];
  const params = (s) => [...String(s).matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort().join(',');
  const walk = (r, e, p) => {
    for (const k of Object.keys(r)) {
      const a = r[k], b = e[k], key = p + k;
      if (typeof a === 'string') {
        const pb = typeof b === 'string' ? params(b) : Object.values(b || {}).map(params)[0];
        if (params(a) !== pb) bad.push(`${key}: ${params(a)} ≠ ${pb}`);
      } else if (a && 'one' in a && 'few' in a) {
        if (!(b && 'one' in b && 'other' in b)) bad.push(`${key}: en без one/other`);
        if (!('many' in a)) bad.push(`${key}: ru без many`);
      } else if (a && typeof a === 'object') walk(a, b || {}, key + '.');
    }
  };
  walk(DICTS.ru, DICTS.en, '');
  assert.deepEqual(bad, []);
});

// Все строки интерфейса — в словарях: кириллица в коде веба допустима только
// в комментариях (и в этом тесте/других тестах).
const SRC = new URL('..', import.meta.url).pathname;
function sources(dir) {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n);
    if (statSync(p).isDirectory()) return n === 'i18n' ? [] : sources(p);
    return /\.(svelte|js)$/.test(n) && !/\.test\.js$/.test(n) ? [p] : [];
  });
}
function stripComments(src) {
  return src
    .replace(/<!--[\s\S]*?-->/g, '')
    .replace(/\/\*[\s\S]*?\*\//g, '')
    .replace(/(^|[\s;,{}()])\/\/.*$/gm, '$1');
}

test('в коде веба нет кириллицы вне словарей (кроме комментариев)', () => {
  const bad = [];
  for (const f of sources(SRC)) {
    stripComments(readFileSync(f, 'utf8')).split('\n').forEach((l, i) => {
      if (/[А-Яа-яЁё]/.test(l)) bad.push(`${f.slice(SRC.length)}: ${l.trim()}`);
    });
  }
  assert.deepEqual(bad, [], 'вынесите строки в src/lib/i18n/{ru,en}/*.js:\n' + bad.join('\n'));
});
