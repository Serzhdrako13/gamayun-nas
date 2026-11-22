import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirNameError, winReservedName } from './names.js';

// Общие примеры с Go (internal/nas/winname_test.go): те же решения и тексты.
const cases = JSON.parse(readFileSync(new URL('../../../internal/nas/testdata/winnames.json', import.meta.url), 'utf8'));

test('допустимые имена', () => {
  assert.ok(cases.valid.length > 0);
  for (const n of cases.valid) assert.equal(dirNameError(n), '', n);
});

test('недопустимые имена — тот же текст, что в gamayund', () => {
  assert.ok(cases.invalid.length > 0);
  for (const c of cases.invalid) assert.equal(dirNameError(c.name), c.error, JSON.stringify(c.name));
});

test('имена устройств Windows', () => {
  for (const [n, want] of [['con', 'CON'], ['CON.txt', 'CON'], ['con .x', 'CON'], ['Com3', 'COM3'], ['lpt²', 'LPT²'],
    ['console', ''], ['com10', ''], ['COM0', ''], ['', ''], ['.con', ''], ['конь', ''], ['docs', '']]) {
    assert.equal(winReservedName(n), want, n);
  }
});
