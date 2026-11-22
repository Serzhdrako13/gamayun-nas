// Набор компонентов компилируется, без своих <style> (CSP: стили —
// только внешний CSS) и без инлайн-атрибута style= (только директивы style:).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { join } from 'node:path';
import { compile } from 'svelte/compiler';

const DIR = new URL('../ui', import.meta.url).pathname;
const comps = readdirSync(DIR).filter((f) => f.endsWith('.svelte'));

test('набор компонентов полон', () => {
  for (const n of ['Window', 'Widget', 'Table', 'Props', 'Status', 'Ring', 'Meter', 'Tabs']) assert.ok(comps.includes(n + '.svelte'), n);
});

for (const f of comps) {
  test(`компонент ${f}`, () => {
    const src = readFileSync(join(DIR, f), 'utf8');
    assert.ok(!/<style/.test(src), 'свой <style> запрещён — стили в ui.css');
    assert.ok(!/\sstyle="/.test(src), 'инлайн style="" запрещён CSP — используйте style:');
    const r = compile(src, { filename: f, generate: 'client' });
    assert.deepEqual(r.warnings.map((w) => w.code), [], 'предупреждения компилятора');
  });
}
