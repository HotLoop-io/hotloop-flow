// The editor's words, in the language the person picked.
//
// English is part of the bundle and is what every missing word falls back to.
// The other twelve are separate files, loaded when asked for, so a person who
// reads English never downloads the rest. A catalog is flat: a key, and the text
// with {name} where a value goes. A count that changes the wording is a family
// of keys, key_one and key_other (and key_zero, key_two, key_few, key_many for a
// language that needs them), chosen by the language's own plural rules.
//
// Numbers, dates and lists go through Intl, never through string building, so a
// person in Hindi gets Devanagari-region grouping and a person in Arabic gets
// the right order.

import en from '../i18n/ui.en.json';

export type Messages = Record<string, string>;

export interface Language {
  code: string;
  /** The language written in itself, which is what the picker shows. */
  name: string;
  rtl?: boolean;
}

/** In the order the picker shows them: English, then the order Patrick gave. */
export const LANGUAGES: readonly Language[] = [
  { code: 'en', name: 'English' },
  { code: 'es', name: 'Español' },
  { code: 'fr', name: 'Français' },
  { code: 'it', name: 'Italiano' },
  { code: 'de', name: 'Deutsch' },
  { code: 'nb', name: 'Norsk bokmål' },
  { code: 'bg', name: 'Български' },
  { code: 'el', name: 'Ελληνικά' },
  { code: 'ko', name: '한국어' },
  { code: 'ja', name: '日本語' },
  { code: 'vi', name: 'Tiếng Việt' },
  { code: 'hi', name: 'हिन्दी' },
  { code: 'zh-Hans', name: '简体中文' },
  { code: 'ar', name: 'العربية', rtl: true },
];

const loaders: Record<string, () => Promise<{ default: Messages }>> = {
  es: () => import('../i18n/ui.es.json'),
  fr: () => import('../i18n/ui.fr.json'),
  it: () => import('../i18n/ui.it.json'),
  de: () => import('../i18n/ui.de.json'),
  nb: () => import('../i18n/ui.nb.json'),
  bg: () => import('../i18n/ui.bg.json'),
  el: () => import('../i18n/ui.el.json'),
  ko: () => import('../i18n/ui.ko.json'),
  ja: () => import('../i18n/ui.ja.json'),
  vi: () => import('../i18n/ui.vi.json'),
  hi: () => import('../i18n/ui.hi.json'),
  'zh-Hans': () => import('../i18n/ui.zh-Hans.json'),
  ar: () => import('../i18n/ui.ar.json'),
};

const STORE_KEY = 'hotloop-flow.lang';

/** Maps what a browser says ("de-AT", "zh-CN", "no") to a language we have. */
export function match(tag: string): string | null {
  const t = tag.trim().toLowerCase();
  if (!t || t === '*') return null;
  if (t === 'zh' || t === 'zh-cn' || t === 'zh-sg' || t.startsWith('zh-hans')) return 'zh-Hans';
  if (t.startsWith('zh-')) return null; // Traditional Chinese is not one of ours: try the next language
  if (t === 'no' || t.startsWith('no-') || t === 'nn' || t.startsWith('nn-')) return 'nb';
  const base = t.split('-')[0]!;
  return LANGUAGES.find((l) => l.code.toLowerCase() === base)?.code ?? null;
}

/** The remembered choice, else the browser's first language we speak, else English. */
export function detect(): string {
  try {
    const stored = localStorage.getItem(STORE_KEY);
    if (stored && LANGUAGES.some((l) => l.code === stored)) return stored;
  } catch {
    /* private mode */
  }
  for (const tag of typeof navigator !== 'undefined' ? navigator.languages ?? [navigator.language] : []) {
    const code = match(tag);
    if (code) return code;
  }
  return 'en';
}

const state = $state({ lang: 'en', messages: {} as Messages, rev: 0 });
const listeners = new Set<() => void>();

/** The code of the language on screen. Reading it in a template makes the template follow the picker. */
export const lang = (): string => state.lang;
export const isRTL = (): boolean => LANGUAGES.find((l) => l.code === state.lang)?.rtl === true;

function applyDocument(code: string): void {
  if (typeof document === 'undefined') return;
  const l = LANGUAGES.find((x) => x.code === code);
  document.documentElement.lang = code;
  document.documentElement.dir = l?.rtl ? 'rtl' : 'ltr';
  document.title = tr('app.title');
}

/**
 * Switches language. Resolves once its words are loaded, so a caller can draw
 * after it. A file that cannot be fetched leaves the person on English rather
 * than on a half-translated screen.
 */
export async function setLanguage(code: string, remember = true): Promise<void> {
  if (!LANGUAGES.some((l) => l.code === code)) code = 'en';
  let messages: Messages = {};
  if (code !== 'en') {
    try {
      messages = (await loaders[code]!()).default;
    } catch {
      code = 'en';
    }
  }
  state.messages = messages;
  state.lang = code;
  state.rev++;
  if (remember) {
    try {
      localStorage.setItem(STORE_KEY, code);
    } catch {
      /* private mode */
    }
  }
  applyDocument(code);
  for (const fn of listeners) fn();
}

/** The first thing the app does: show the remembered or detected language. */
export async function initLanguage(): Promise<void> {
  await setLanguage(detect(), false);
}

/** Calls back after every language change, for the parts drawn by hand rather than by a template. */
export function onLanguageChange(fn: () => void): () => void {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

const ISOLATE_OPEN = '⁨';
const ISOLATE_CLOSE = '⁩';

function fill(text: string, params?: Record<string, string | number>): string {
  if (!params) return text;
  const rtl = isRTL();
  return text.replace(/\{(\w+)\}/g, (whole, name: string) => {
    const v = params[name];
    if (v === undefined) return whole;
    const s = typeof v === 'number' ? fmtNumber(v) : String(v);
    // A node's name or a path inside an Arabic sentence would otherwise reorder
    // the words around it.
    return rtl ? ISOLATE_OPEN + s + ISOLATE_CLOSE : s;
  });
}

/**
 * The text for a key. It reads the language, so a template that calls it draws
 * again when the language changes. A key the language lacks falls back to
 * English, and a key nobody has shows as itself so that it is seen and fixed.
 */
export function tr(key: string, params?: Record<string, string | number>): string {
  void state.rev;
  const text = state.messages[key] ?? (en as Messages)[key] ?? key;
  return fill(text, params);
}

/** The text for a key when there is one, else the fallback: for names that data can add to (a subflow's own category). */
export function trOr(key: string, fallback: string): string {
  void state.rev;
  return key in state.messages || key in (en as Messages) ? tr(key) : fallback;
}

/** A counted phrase: the key without its plural suffix, and the number. The text comes from the form the language's plural rules pick. */
export function trn(key: string, n: number, params: Record<string, string | number> = {}): string {
  void state.rev;
  let category = 'other';
  try {
    category = new Intl.PluralRules(state.lang).select(n);
  } catch {
    /* an unknown tag: English rules */
  }
  const found = [`${key}_${category}`, `${key}_other`].find((k) => k in state.messages || k in en);
  return tr(found ?? `${key}_other`, { n, ...params });
}

// ── Intl ──────────────────────────────────────────────────────────────────────
// Always the picked language's own conventions, and the Latin digits for Arabic
// and Hindi: a plant reads a setpoint as 1450, not as ١٤٥٠ or १४५०, and a flow
// that says 1450 in one place must not say something else in another.
const intlTag = (): string => {
  const code = state.lang === 'ar' ? 'ar-u-nu-latn' : state.lang === 'hi' ? 'hi-u-nu-latn' : state.lang;
  return code;
};

export const fmtNumber = (n: number, opts?: Intl.NumberFormatOptions): string => {
  void state.rev;
  try {
    return new Intl.NumberFormat(intlTag(), opts).format(n);
  } catch {
    return String(n);
  }
};

/**
 * A number with a fixed count of decimals and no digit grouping, which is how a
 * reading is shown on a panel: 1450.00 in English, 1450,00 in German. Grouping
 * is left out so a value reads the same width as it changes.
 */
export const fmtFixed = (n: number, digits: number): string =>
  fmtNumber(n, { minimumFractionDigits: digits, maximumFractionDigits: digits, useGrouping: false });

export const fmtDate = (d: Date | number | string, opts: Intl.DateTimeFormatOptions = { dateStyle: 'medium', timeStyle: 'medium' }): string => {
  void state.rev;
  const date = d instanceof Date ? d : new Date(d);
  if (Number.isNaN(date.getTime())) return String(d);
  try {
    return new Intl.DateTimeFormat(intlTag(), opts).format(date);
  } catch {
    return date.toISOString();
  }
};

export const fmtTime = (d: Date | number | string): string => fmtDate(d, { timeStyle: 'medium' });

/** "2 minutes ago", "in 3 hours", in the person's language. */
export const fmtRelative = (seconds: number): string => {
  void state.rev;
  const abs = Math.abs(seconds);
  const units: [Intl.RelativeTimeFormatUnit, number][] = [['day', 86400], ['hour', 3600], ['minute', 60], ['second', 1]];
  const [unit, size] = units.find(([, s]) => abs >= s) ?? ['second', 1];
  try {
    return new Intl.RelativeTimeFormat(intlTag(), { numeric: 'auto' }).format(Math.round(seconds / size), unit);
  } catch {
    return `${Math.round(seconds / size)} ${unit}`;
  }
};

/** "a, b, and c" the way the language writes it. */
export const fmtList = (items: string[]): string => {
  void state.rev;
  try {
    return new Intl.ListFormat(intlTag(), { style: 'long', type: 'conjunction' }).format(items);
  } catch {
    return items.join(', ');
  }
};

// ── What the runtime says ─────────────────────────────────────────────────────
// The admin API answers in English, with the numbers and names of the case in
// the sentence. serverText finds the sentence in the catalog (under
// "server.<the English>", with {1}, {2} where the values were) and says it in
// the person's language. A sentence the catalog does not know is shown as the
// runtime wrote it, because a plain English error is better than none.
let serverPatterns: { re: RegExp; key: string }[] | null = null;
let serverFor = '';

function compileServer(): { re: RegExp; key: string }[] {
  const out: { re: RegExp; key: string }[] = [];
  for (const key of Object.keys(en as Messages)) {
    if (!key.startsWith('server.')) continue;
    const english = (en as Messages)[key]!;
    if (!/\{\d+\}/.test(english)) continue;
    const escaped = english.replace(/[.*+?^$()|[\]\\]/g, '\\$&').replace(/\{\d+\}/g, '(.+?)');
    out.push({ re: new RegExp(`^${escaped}$`, 's'), key });
  }
  return out;
}

export function serverText(message: string): string {
  void state.rev;
  if (state.lang === 'en') return message;
  const exactKey = Object.keys(en as Messages).find((k) => k.startsWith('server.') && (en as Messages)[k] === message);
  if (exactKey) return tr(exactKey);
  if (serverPatterns === null || serverFor !== 'en') {
    serverPatterns = compileServer();
    serverFor = 'en';
  }
  for (const p of serverPatterns) {
    const m = p.re.exec(message);
    if (!m) continue;
    const params: Record<string, string> = {};
    m.slice(1).forEach((v, i) => { params[String(i + 1)] = v; });
    const text = state.messages[p.key] ?? (en as Messages)[p.key]!;
    return fillNumbered(text, params);
  }
  return message;
}

function fillNumbered(text: string, params: Record<string, string>): string {
  const rtl = isRTL();
  return text.replace(/\{(\d+)\}/g, (whole, n: string) => {
    const v = params[n];
    if (v === undefined) return whole;
    return rtl ? ISOLATE_OPEN + v + ISOLATE_CLOSE : v;
  });
}
