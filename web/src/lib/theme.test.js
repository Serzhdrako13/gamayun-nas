// Цвета задаются только токенами тем (src/themes/*.css). Экраны,
// компоненты и app.css литералов цвета не содержат — иначе новая тема
// (светлая) ломается незаметно. Обе темы задают одинаковый набор токенов.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync, statSync } from 'node:fs';
import { join } from 'node:path';

const SRC = new URL('..', import.meta.url).pathname;
const COLOR = /#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklch|oklab)\(|\b(?:white|black)\b(?=\s*[;}])/;

function files(dir) {
  return readdirSync(dir).flatMap((n) => {
    const p = join(dir, n);
    if (statSync(p).isDirectory()) return n === 'themes' ? [] : files(p);
    return /\.(svelte|css)$/.test(n) ? [p] : [];
  });
}

test('нет литералов цвета вне тем', () => {
  const bad = [];
  for (const f of files(SRC)) {
    readFileSync(f, 'utf8').split('\n').forEach((l, i) => {
      const code = l.replace(/\/\*.*?\*\/|<!--.*?-->/g, '');
      if (COLOR.test(code) && !/url\(#/.test(code)) bad.push(`${f.slice(SRC.length)}:${i + 1}: ${l.trim()}`);
    });
  }
  assert.deepEqual(bad, [], 'используйте токены темы:\n' + bad.join('\n'));
});

test('темы задают одинаковые токены', () => {
  const vars = (f) => new Set([...readFileSync(join(SRC, 'themes', f), 'utf8').matchAll(/^\s*(--[\w-]+)\s*:/gm)].map((m) => m[1]));
  const d = vars('dark.css'), l = vars('light.css');
  assert.deepEqual([...d].filter((v) => !l.has(v)), [], 'нет в light.css');
  assert.deepEqual([...l].filter((v) => !d.has(v)), [], 'нет в dark.css');
});
