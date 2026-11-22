// тексты справки, собранные Vite из help/<язык>/<id>.md.
// Разбор — в parse.js (тестируется в node без Vite).
import { buildIndex, sectionIn, sectionsIn, searchIn } from './parse.js';
import { lang } from '../lib/i18n/index.js';

const INDEX = buildIndex(import.meta.glob('./*/*.md', { query: '?raw', import: 'default', eager: true }));

export const helpSection = (id) => sectionIn(INDEX, id, lang());
export const hasHelp = (id) => !!id && !!sectionIn(INDEX, id, lang());
export const helpSections = () => sectionsIn(INDEX, lang());
export const searchHelp = (q) => searchIn(INDEX, q, lang());
