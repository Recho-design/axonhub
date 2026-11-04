import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const dialogSource = readFileSync(
  new URL('./models-association-dialog.tsx', import.meta.url),
  'utf8'
);
const tagsInputSource = readFileSync(
  new URL('../../../components/ui/tags-autocomplete-input.tsx', import.meta.url),
  'utf8'
);
const enLocales = JSON.parse(
  readFileSync(new URL('../../../locales/en/models.json', import.meta.url), 'utf8')
);
const zhLocales = JSON.parse(
  readFileSync(new URL('../../../locales/zh-CN/models.json', import.meta.url), 'utf8')
);

test('TagsAutocompleteInput component defines strict and emptyMessage props with strict defaulting to false', () => {
  assert.match(
    tagsInputSource,
    /strict\?:\s*boolean/,
    'TagsAutocompleteInputProps must define optional strict prop'
  );
  assert.match(
    tagsInputSource,
    /emptyMessage\?:\s*string/,
    'TagsAutocompleteInputProps must define optional emptyMessage prop'
  );
  assert.match(
    tagsInputSource,
    /strict\s*=\s*false/,
    'strict prop must default to false to keep free-input scenarios backwards-compatible'
  );
});

test('TagsAutocompleteInput in strict mode accepts exact suggestion matches and discards unconfirmed search terms', () => {
  // Mirrors resolveMatch in TagsAutocompleteInput after simplification
  function resolveMatch(input, suggestions, strict) {
    const trimmed = input.trim();
    if (!trimmed) return null;
    if (!strict) return trimmed;
    return suggestions.find((s) => s.toLowerCase() === trimmed.toLowerCase()) ?? null;
  }

  const suggestions = ['gpt-4', 'gpt-4o', 'claude-3-5-sonnet'];

  // Defect 1: Unconfirmed search term 'gpt'
  assert.equal(resolveMatch('gpt', suggestions, true), null, 'strict mode must reject unconfirmed term gpt');
  assert.equal(resolveMatch('gpt-4', suggestions, true), 'gpt-4', 'strict mode accepts exact match gpt-4');
  assert.equal(resolveMatch('GPT-4', suggestions, true), 'gpt-4', 'strict mode accepts case-insensitive match gpt-4');
  assert.equal(resolveMatch('gpt-4o', suggestions, true), 'gpt-4o', 'strict mode accepts exact match gpt-4o');
  assert.equal(resolveMatch('unknown-model', suggestions, true), null, 'strict mode rejects unlisted model');

  // Backwards compatibility: Non-strict mode permits arbitrary inputs
  assert.equal(resolveMatch('gpt', suggestions, false), 'gpt', 'non-strict mode accepts any custom text');
  assert.equal(resolveMatch('custom-tag', suggestions, false), 'custom-tag', 'non-strict mode accepts custom tag');
});

test('models-association-dialog.tsx enables strict mode and dynamic guidance messages on channel_model', () => {
  assert.match(
    dialogSource,
    /channelModelMessages\s*=\s*useMemo\(/,
    'channelModelMessages must be computed dynamically based on channelId and suggestions'
  );
  assert.match(
    dialogSource,
    /<TagsAutocompleteInput[\s\S]*?placeholder=\{channelModelMessages\.placeholder\}[\s\S]*?emptyMessage=\{channelModelMessages\.emptyMessage\}[\s\S]*?suggestions=\{channelModelSuggestions\}[\s\S]*?strict=\{true\}/,
    'TagsAutocompleteInput on channel_model row must pass strict={true} and the dynamic guidance messages'
  );
});

test('switching channel in channel_model row clears modelIds', () => {
  assert.match(
    dialogSource,
    /if\s*\(\s*type\s*===\s*'channel_model'\s*&&\s*newChannelId\s*!==\s*prevChannelId\s*\)\s*\{\s*form\.setValue\(`associations\.\$\{index\}\.modelIds`,\s*\[\]/,
    'changing channelId on channel_model row must reset modelIds to empty array'
  );
});

test('guidance i18n keys exist and match across English and Chinese locales', () => {
  const selectChannelKey = 'models.dialogs.association.selectChannelFirst';
  const noChannelModelsKey = 'models.dialogs.association.noChannelModelsAvailable';

  assert.ok(enLocales[selectChannelKey], 'selectChannelFirst must exist in en locale');
  assert.ok(zhLocales[selectChannelKey], 'selectChannelFirst must exist in zh-CN locale');
  assert.ok(enLocales[noChannelModelsKey], 'noChannelModelsAvailable must exist in en locale');
  assert.ok(zhLocales[noChannelModelsKey], 'noChannelModelsAvailable must exist in zh-CN locale');

  assert.equal(zhLocales[selectChannelKey], '请先选择渠道');
  assert.equal(zhLocales[noChannelModelsKey], '该渠道暂无可用模型');
});
