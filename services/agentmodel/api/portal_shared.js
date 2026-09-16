/* Shared by portal.html and portal_admin.html; spliced into each page's nonce'd script element
   by servePortalHTML. Pages extend ERRORS with their own codes before calling explain(). */
const $ = id => document.getElementById(id);
const number = value => Number(value || 0).toLocaleString('en-US');
// Figures at or above one billion are shown as "1.2B" so the ledger keeps its width; the
// exact count stays in the title so hovering reveals it.
function setFigure(element, value) {
  const n = Number(value || 0);
  if (n >= 1e9) { element.textContent = (n / 1e9).toFixed(1) + 'B'; element.title = number(n); }
  else { element.textContent = number(n); element.removeAttribute('title'); }
}
const THEME_KEY = 'agentmodel-portal-theme';
const HOVERABLE = matchMedia('(hover: hover)').matches;

const ERRORS = {
  access_assertion_required: 'Your sign-in session is missing or expired. Reload the page to sign in again.',
  csrf_rejected: 'The security token did not match. Reload the page and retry.',
  store_unavailable: 'The gateway store is unavailable. Try again shortly.',
  portal_unavailable: 'The portal is not configured on this gateway.',
  invalid_reporting_timezone: 'This portal requires Asia/Taipei reporting. Reload the page and retry.',
  invalid_host: 'This page was opened on an unexpected host.',
  session_not_json: 'The gateway did not answer with JSON; your sign-in session may have expired. Reload the page.'
};
function explain(code) {
  return ERRORS[code] || ('Request failed (' + code + ').');
}
function apiError(code, status) {
  const error = new Error(explain(code));
  error.code = code;
  error.status = status;
  return error;
}
async function api(path, init) {
  const response = await fetch(path, Object.assign({cache: 'no-store', credentials: 'same-origin'}, init || {}));
  let data;
  try { data = await response.json(); } catch (_) { throw apiError('session_not_json', response.status); }
  if (!response.ok) throw apiError(data && data.error ? data.error : 'http_' + response.status, response.status);
  return data;
}
/* Status lines: message plus tone; an optional code is appended as <code> for support. */
function announce(id, message, tone, code) {
  const status = $(id);
  status.replaceChildren();
  status.textContent = message;
  if (code) {
    const tag = document.createElement('code');
    tag.textContent = ' ' + code;
    status.append(tag);
  }
  status.setAttribute('data-tone', tone || 'info');
}

/* Theme: the only persisted preference. "system" clears the override. */
function applyTheme(mode) {
  const root = document.documentElement;
  if (mode === 'light' || mode === 'dark') root.setAttribute('data-theme', mode);
  else root.removeAttribute('data-theme');
  for (const button of document.querySelectorAll('[data-theme-choice]')) {
    button.setAttribute('aria-pressed', String(button.getAttribute('data-theme-choice') === (mode || 'system')));
  }
}
function initTheme() {
  let saved = 'system';
  try { saved = localStorage.getItem(THEME_KEY) || 'system'; } catch (_) {}
  applyTheme(saved);
  for (const button of document.querySelectorAll('[data-theme-choice]')) {
    button.onclick = () => {
      const mode = button.getAttribute('data-theme-choice');
      try { if (mode === 'system') localStorage.removeItem(THEME_KEY); else localStorage.setItem(THEME_KEY, mode); } catch (_) {}
      applyTheme(mode);
    };
  }
}

/* Tooltip placement: centered above the anchor, clamped inside the tip's .chart container so
   the first and last columns stay readable. `half` is the tooltip's half width in px. */
function placeTip(tip, anchor, half) {
  const chart = tip.parentElement.getBoundingClientRect(), box = anchor.getBoundingClientRect();
  tip.style.left = Math.min(Math.max(box.left + box.width / 2 - chart.left, half), Math.max(half, chart.width - half)) + 'px';
  tip.style.top = (box.top - chart.top) + 'px';
  tip.hidden = false;
}

/* Portal wall time is fixed Taipei UTC+8, never the browser's local zone. UTC getters
   below serialize shifted civil fields, not instants; only taipeiInstant parses input. */
const PORTAL_TIMEZONE = 'Asia/Taipei';
const PORTAL_TIME_LABEL = 'Asia/Taipei (UTC+8)';
const TAIPEI_OFFSET_MS = 8 * 60 * 60 * 1000;
function taipeiInput(value) {
  if (!value) return '';
  const at = new Date(value);
  return isNaN(at) ? '' : new Date(at.getTime() + TAIPEI_OFFSET_MS).toISOString().slice(0, 19);
}
function taipeiInstant(value) {
  return new Date(value + '+08:00').toISOString().replace('.000Z', 'Z');
}
function stamp(value) {
  const civil = taipeiInput(value);
  return civil ? civil.replace('T', ' ').slice(0, 16) : String(value);
}

async function copyText(button, text) {
  const label = button.textContent;
  try {
    await navigator.clipboard.writeText(text);
    button.textContent = 'Copied';
  } catch (_) {
    button.textContent = 'Select and copy manually';
  }
  setTimeout(() => { button.textContent = label; }, 1800);
}
