import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import test from 'node:test';
import ts from 'typescript';

const output = ts.transpileModule(readFileSync(join(import.meta.dirname, 'keep-alive.ts'), 'utf8'), {
  compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2023 },
}).outputText;
const { keepAliveValidation } = await import(`data:text/javascript;base64,${Buffer.from(output).toString('base64')}`);

const valid = {
  streaming: { enabled: true, intervalSeconds: 15 },
  nonStreaming: { enabled: true, intervalSeconds: 15 },
};

test('keepAliveValidation accepts positive finite intervals', () => {
  assert.equal(keepAliveValidation(valid), null);
  assert.equal(
    keepAliveValidation({
      streaming: { enabled: false, intervalSeconds: 0.1 },
      nonStreaming: { enabled: false, intervalSeconds: 99.9 },
    }),
    null
  );
});

test('keepAliveValidation rejects non-positive or non-finite streaming intervals', () => {
  for (const intervalSeconds of [0, -1, NaN, Infinity, -Infinity]) {
    assert.equal(
      keepAliveValidation({
        ...valid,
        streaming: { enabled: true, intervalSeconds },
      }),
      'streamingInterval'
    );
  }
});

test('keepAliveValidation rejects non-positive or non-finite non-streaming intervals', () => {
  for (const intervalSeconds of [0, -5, NaN, Infinity, -Infinity]) {
    assert.equal(
      keepAliveValidation({
        ...valid,
        nonStreaming: { enabled: true, intervalSeconds },
      }),
      'nonStreamingInterval'
    );
  }
});

test('keepAlive translations match in both locales', () => {
  const root = join(import.meta.dirname, '../../../locales');
  const en = JSON.parse(readFileSync(join(root, 'en/system.json'), 'utf8'));
  const zh = JSON.parse(readFileSync(join(root, 'zh-CN/system.json'), 'utf8'));
  const keys = Object.keys(en).filter((key) => key.startsWith('system.keepAlive.') || key === 'system.tabs.keepAlive');
  assert.ok(keys.length >= 15, `Expected >= 15 keys, got ${keys.length}`);
  for (const key of keys) {
    assert.equal(typeof zh[key], 'string', `Missing translation for ${key} in zh-CN`);
  }
});

// Regression tripwire: every literal t('...') key used by the settings
// component must resolve in both locales (catches references to shared keys
// like common.buttons.* that do not exist, which render as raw keys in UI).
test('keep-alive-settings component keys resolve in both locales', () => {
  const root = join(import.meta.dirname, '..');
  const component = readFileSync(join(root, 'components/keep-alive-settings.tsx'), 'utf8');
  const used = [...component.matchAll(/\bt\('([a-zA-Z.]+)'\)/g)].map((m) => m[1]);
  assert.ok(used.length >= 20, `Expected >= 20 t() keys, got ${used.length}`);

  const loadLocale = (lang) =>
    Object.assign(
      {},
      ...['base', 'system'].map((file) =>
        JSON.parse(readFileSync(join(root, `../../locales/${lang}/${file}.json`), 'utf8'))
      )
    );
  const locales = { en: loadLocale('en'), 'zh-CN': loadLocale('zh-CN') };

  for (const key of new Set(used)) {
    for (const [lang, table] of Object.entries(locales)) {
      assert.equal(typeof table[key], 'string', `Key ${key} used by keep-alive-settings is missing in ${lang}`);
    }
  }
});
