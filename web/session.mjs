// Read CPA's existing browser login for this server only. Never persist keys or
// send a key to the backend selected for another CPA panel / reverse-proxy path.
export function normalizeBase(value) {
  if (typeof value !== 'string' || !value.trim()) return '';
  try {
    const raw = value.trim().replace(/\/?v0\/management\/?$/i, '').replace(/\/+$/, '');
    const url = new URL(/^https?:\/\//i.test(raw) ? raw : 'http://' + raw);
    if (!['http:', 'https:'].includes(url.protocol) || url.username || url.password || url.search || url.hash) return '';
    return url.href.replace(/\/+$/, '');
  } catch { return ''; }
}

export function resourceBase(href) {
  const url = new URL(href);
  const marker = '/v0/resource/plugins/codex-quota-scheduler/';
  const index = url.pathname.lastIndexOf(marker);
  if (index < 0) throw new Error('请通过 CPA 的 Codex 额度调度菜单打开此页面。');
  return normalizeBase(url.origin + url.pathname.slice(0, index));
}

export function decodeStored(raw, host, userAgent) {
  if (!raw) return null;
  try {
    if (raw.startsWith('enc::v1::')) {
      const binary = atob(raw.slice('enc::v1::'.length));
      const salt = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + host + '|' + userAgent);
      raw = new TextDecoder().decode(Uint8Array.from(binary, (c, i) => c.charCodeAt(0) ^ salt[i % salt.length]));
    }
    try { return JSON.parse(raw); } catch { return raw; }
  } catch { return null; }
}

export function validKey(value) {
  return typeof value === 'string' && value.trim() && value.length <= 4096 && !/[\r\n]/.test(value) ? value.trim() : '';
}

export function readPanelKey(storage, base, host, userAgent) {
  const read = key => {
    try { return decodeStored(storage.getItem(key), host, userAgent); } catch { return null; }
  };
  const unwrap = value => value && typeof value === 'object' ? value.state || value : null;
  const normalized = normalizeBase(base);
  if (!normalized) return '';
  const scope = encodeURIComponent(normalized);
  const selected = read('cli-proxy-auth:selection:' + scope);
  if (selected !== null) {
    if (normalizeBase(selected) !== normalized) return '';
    const state = unwrap(read('cli-proxy-auth:scope:' + scope + ':' + scope));
    if (!state || (state.apiBase && normalizeBase(state.apiBase) !== normalized)) return '';
    return validKey(state.managementKey);
  }
  const state = unwrap(read('cli-proxy-auth'));
  if (state) return normalizeBase(state.apiBase) === normalized ? validKey(state.managementKey) : '';
  // Older CPA panels stored the endpoint and key in separate entries.
  if (normalizeBase(read('apiBase') || read('apiUrl')) !== normalized) return '';
  return validKey(read('managementKey'));
}
