// Клиент API gamayund: cookie сессии + заголовок X-CSRF-Token для небезопасных методов.
// Текст ошибки с сервера (error.message) — как есть, по-русски.
import { t } from './i18n/index.js';

let csrf = '';
let onUnauthorized = () => {};

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export function setCsrf(token) {
  csrf = token || '';
}

// csrfToken — для запросов мимо api() (загрузка файлов с прогрессом).
export const csrfToken = () => csrf;

export function setUnauthorizedHandler(fn) {
  onUnauthorized = fn;
}

export async function api(method, path, body) {
  const headers = {};
  const opts = { method, headers, credentials: 'same-origin' };
  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  if (method !== 'GET' && method !== 'HEAD') {
    headers['X-CSRF-Token'] = csrf;
    if (body === undefined) headers['Content-Type'] = 'application/json';
  }
  let res;
  try {
    res = await fetch('/api/v1' + path, opts);
  } catch {
    throw new ApiError(0, 'network', t('app.api.network'));
  }
  if (res.status === 204) return null;
  let data = null;
  try {
    data = await res.json();
  } catch {
    /* пустое или не-JSON тело */
  }
  if (!res.ok) {
    const e = data && data.error ? data.error : {};
    const err = new ApiError(res.status, e.code || 'http_' + res.status, e.message || t('app.api.failed', { status: res.status }));
    if (res.status === 401 && !path.startsWith('/auth/login')) onUnauthorized(err);
    throw err;
  }
  return data;
}

export const get = (p) => api('GET', p);
export const post = (p, b) => api('POST', p, b ?? {});
export const put = (p, b) => api('PUT', p, b);
export const patch = (p, b) => api('PATCH', p, b);
export const del = (p, b) => api('DELETE', p, b);
