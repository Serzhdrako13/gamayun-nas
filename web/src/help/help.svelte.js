// состояние окна справки — открыто/свёрнуто/«на весь экран», раздел,
// история «Назад», положение и размер. Хранится в localStorage (dn.help.v1):
// после перезагрузки окно открывается там же. Окно поверх страницы —
// несохранённое на странице не теряется.
const KEY = 'dn.help.v1';
const DEFAULT = { open: false, min: false, max: false, section: 'overview', history: [], x: null, y: null, w: null, h: null };

function load() {
  try {
    const raw = localStorage.getItem(KEY);
    if (raw) {
      const v = { ...DEFAULT, ...JSON.parse(raw) };
      if (!Array.isArray(v.history)) v.history = [];
      return v;
    }
  } catch {
 /* хранилище недоступно — по умолчанию */
  }
  return { ...DEFAULT };
}

export const help = $state(load());

$effect.root(() => {
  $effect(() => {
    const s = JSON.stringify(help);
    try {
      localStorage.setItem(KEY, s);
    } catch {
 /* не сохранилось — не страшно */
    }
  });
});

const push = (id) => (help.history = [...help.history, id].slice(-20));

// openHelp — открыть раздел (кнопка «?»): прежний раздел уходит в историю.
export function openHelp(id) {
  if (help.open && help.section !== id) push(help.section);
  help.section = id;
  help.open = true;
  help.min = false;
}
// goTo — ссылка внутри справки.
export function goTo(id) {
  if (id === help.section) return;
  push(help.section);
  help.section = id;
}
export function back() {
  if (!help.history.length) return;
  help.section = help.history[help.history.length - 1];
  help.history = help.history.slice(0, -1);
}
export const minimize = () => (help.min = true);
export const restore = () => (help.min = false);
export const toggleMax = () => (help.max = !help.max);
export function close() {
  help.open = false;
  help.min = false;
  help.history = [];
}
export function moveTo(x, y) {
  help.x = x;
  help.y = y;
}
export function resizeTo(w, h) {
  help.w = w;
  help.h = h;
}
