/**
 * Shared helpers for validating backend (Go regexp) patterns in the frontend.
 *
 * The backend compiles patterns with Go's regexp package, whose inline
 * modifiers like (?i) are not valid JavaScript RegExp syntax. These helpers
 * translate Go-only constructs to JS equivalents so client-side validation
 * accepts every pattern the backend accepts.
 */

/** Inline modifiers at the start of a pattern, e.g. "(?is)". */
const INLINE_MODIFIER_RE = /^\(\?([a-zA-Z]+)\)/;

// JS RegExp flags corresponding to Go inline modifier letters.
const GO_FLAG_TO_JS: Record<string, string> = { i: 'i', m: 'm', s: 's' };

/** Splits a leading Go inline modifier like "(?i)" into JS flags and the body.
 * A modifier containing letters without a JS equivalent (e.g. "U" ungreedy) is
 * dropped entirely: validation stays lenient because the backend re-validates
 * with the real Go engine on save. */
export function splitInlineModifier(pattern: string): { flags: string; body: string } {
  const match = INLINE_MODIFIER_RE.exec(pattern);
  if (!match) {
    return { flags: '', body: pattern };
  }

  const letters = [...match[1]];
  if (letters.some((flag) => !GO_FLAG_TO_JS[flag])) {
    return { flags: '', body: pattern.slice(match[0].length) };
  }

  return { flags: letters.map((flag) => GO_FLAG_TO_JS[flag]).join(''), body: pattern.slice(match[0].length) };
}

/** Rewrites Go-only named groups (?P<name>...) into JS (?<name>...) form. */
function toJsBody(body: string): string {
  return body.replace(/\(\?P</g, '(?<');
}

/** Returns null when the pattern is valid for the backend, or the error message. */
export function goRegexValidationError(pattern: string): string | null {
  if (!pattern.trim()) {
    return null;
  }

  try {
    const { flags, body } = splitInlineModifier(pattern);
    new RegExp(toJsBody(body), [...new Set(flags)].join(''));
    return null;
  } catch (err) {
    return err instanceof Error ? err.message : 'Invalid regex';
  }
}
