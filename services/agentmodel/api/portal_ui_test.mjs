// Dependency-free checks for the embedded portal pages: node --test portal_ui_test.mjs.
// A minimal DOM stub runs each page's inline script; renderers are then called with fixtures.
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
import test from 'node:test';
import assert from 'node:assert/strict';

// Pages are composed the way servePortalHTML composes them: shared fragments spliced in place.
const read = name => readFileSync(new URL('./' + name, import.meta.url), 'utf8');
const compose = html => html.replace('{{SHARED_CSS}}', read('portal_shared.css')).replace('{{SHARED_JS}}', read('portal_shared.js'));
const portalHTML = compose(read('portal.html'));
const adminHTML = compose(read('portal_admin.html'));
const scriptOf = html => html.match(/<script nonce="\{\{NONCE\}\}">([\s\S]*?)<\/script>/)[1];

const ROOT = {getBoundingClientRect() { return {left: 0, top: 0, width: 0, height: 0}; }};
function makeElement(tag) {
  const e = {
    tag, children: [], style: {}, attributes: {}, classes: [], hidden: false, disabled: false, value: '',
    get textContent() { return e.children.map(n => typeof n === 'string' ? n : n.textContent).join(''); },
    set textContent(value) { e.children = [String(value)]; },
    parent: null,
    classList: {add(name) { if (!e.classes.includes(name)) e.classes.push(name); }, remove(name) { e.classes = e.classes.filter(c => c !== name); }},
    // Fragments splice their children in, as in the DOM.
    append(...nodes) { for (const node of nodes) { if (node && node.tag === '#fragment') { e.append(...node.children); continue; } if (typeof node === 'object') { if (node.parent) node.remove(); node.parent = e; } e.children.push(node); } },
    replaceChildren(...nodes) { e.children = []; e.append(...nodes); },
    replaceWith(node) { if (e.parent) { node.parent = e.parent; e.parent.children = e.parent.children.map(c => c === e ? node : c); } },
    remove() { if (e.parent) e.parent.children = e.parent.children.filter(c => c !== e); e.parent = null; },
    setAttribute(key, value) { e.attributes[key] = String(value); },
    getAttribute(key) { return key in e.attributes ? e.attributes[key] : null; },
    removeAttribute(key) { delete e.attributes[key]; },
    closest(tag) { for (let n = e; n; n = n.parent) { if (n.tag === tag) return n; } return null; },
    // Detached fixtures report the document as their parent, like a real page's body.
    get parentElement() { return e.parent || ROOT; },
    getBoundingClientRect() { return {left: 0, top: 0, width: 0, height: 0}; },
    querySelector(selector) { return e.querySelectorAll(selector)[0] || null; },
    querySelectorAll(selector) { return descendants(e).filter(matcher(selector)); },
  };
  return e;
}
function descendants(e) {
  const out = [];
  for (const child of e.children) { if (typeof child === 'object') { out.push(child, ...descendants(child)); } }
  return out;
}
function matcher(selector) {
  switch (selector) {
    case 'pre': return n => n.tag === 'pre';
    case 'input': return n => n.tag === 'input';
    case 'input:checked': return n => n.tag === 'input' && n.checked === true;
    case 'tr[data-search]': return n => n.tag === 'tr' && 'data-search' in n.attributes;
    case 'a[data-key-id]': return n => n.tag === 'a' && 'data-key-id' in n.attributes;
    default: return () => false;
  }
}

// Snippet blocks are discovered from the HTML so every data-snippet gets a rendered <pre>.
function snippetBlocks(html) {
  return Array.from(html.matchAll(/data-snippet="([^"]+)"/g), m => {
    const block = makeElement('div');
    block.setAttribute('data-snippet', m[1]);
    block.append(makeElement('pre'));
    return block;
  });
}

function page(html, search = '') {
  const listeners = new Map();
  const location = {search, origin: 'https://portal.example.com'};
  const entries = [search];
  let index = 0;
  const history = {
    pushState(_state, _title, url) { location.search = new URL(url, 'https://portal.example.com/portal/admin').search; entries.splice(++index); entries.push(location.search); },
    replaceState(_state, _title, url) { location.search = new URL(url, 'https://portal.example.com/portal/admin').search; entries[index] = location.search; },
    back() { if (index > 0) location.search = entries[--index]; return listeners.get('popstate')?.(); },
  };
  const elements = new Map();
  const blocks = snippetBlocks(html);
  const documentElement = makeElement('html');
  const context = vm.createContext({
    document: {
      documentElement,
      getElementById(id) { if (!elements.has(id)) elements.set(id, makeElement('#' + id)); return elements.get(id); },
      createElement: makeElement,
      createElementNS: (_namespace, tag) => makeElement(tag),
      createTextNode: text => text,
      querySelectorAll: selector => selector === '[data-snippet]' ? blocks : [],
      createDocumentFragment: () => makeElement('#fragment'),
      addEventListener() {},
    },
    matchMedia: () => ({matches: false}),
    window: {addEventListener(name, fn) { listeners.set(name, fn); }},
    location, history,
    navigator: {clipboard: {writeText: async () => {}}},
    localStorage: {getItem: () => null, setItem() {}, removeItem() {}},
    setTimeout: () => 0,
    URLSearchParams,
    confirm: () => true,
    fetch: () => new Promise(() => {}),
  });
  vm.runInContext(scriptOf(html), context);
  const snippet = name => blocks.find(b => b.getAttribute('data-snippet') === name).children[0].textContent;
  return {context, elements, blocks, snippet, documentElement, location, history, listeners, run: code => vm.runInContext(code, context)};
}

test('highlighted snippets preserve exact source and clipboard text after model changes', async () => {
  const {context, blocks, elements, run} = page(portalHTML);
  const copied = [];
  context.navigator.clipboard.writeText = async text => copied.push(text);
  for (const model of ['gpt-first', 'claude-next', '<img src=x onerror=alert(1)>']) {
    elements.get('model-select').value = model;
    elements.get('model-select').onchange();
    for (const block of blocks) {
      const name = block.getAttribute('data-snippet');
      const pre = block.querySelector('pre');
      context.model = model;
      assert.equal(pre.textContent, run(`SNIPPETS[${JSON.stringify(name)}](model)`));
      assert.ok(pre.children.some(n => n.tag === 'span'), name);
      assert.ok(descendants(pre).every(n => n.tag === 'span'));
      context.pre = pre;
      await run('copyText(document.createElement("button"), pre.textContent)');
      assert.equal(copied.at(-1), pre.textContent);
    }
  }
});

test('syntax tokens treat HTML as text and distinguish comments, strings and numbers', () => {
  const {context, run} = page(portalHTML);
  context.pre = makeElement('pre');
  context.source = '# <img src=x onerror=alert(1)>\nif True: print("https://host/<script>", 60.0)';
  run('highlightSnippet(pre, source, "py-code")');
  assert.equal(context.pre.textContent, context.source);
  assert.ok(descendants(context.pre).every(n => n.tag === 'span'));
  const tokens = context.pre.children.filter(n => n.tag === 'span');
  for (const kind of ['comment', 'keyword', 'string', 'number']) {
    assert.ok(tokens.some(n => n.className === 'syntax-' + kind), kind);
  }
  context.source = 'export KEY="$AGENT_MODEL_API_KEY" # local only';
  run('highlightSnippet(pre, source, "setup")');
  assert.equal(context.pre.textContent, context.source);
  assert.ok(descendants(context.pre).some(n => n.className === 'syntax-variable'));
});

test('syntax highlighting remains nonce-contained and uses theme colors without HTML injection', () => {
  assert.equal((portalHTML.match(/<script\b/g) || []).length, 1);
  assert.equal((portalHTML.match(/<style\b/g) || []).length, 1);
  assert.match(portalHTML, /<style nonce="\{\{NONCE\}\}">/);
  assert.doesNotMatch(scriptOf(portalHTML), /innerHTML|insertAdjacentHTML|document\.write|eval\(/);
  assert.doesNotMatch(portalHTML, /<script[^>]+src=|<link[^>]+stylesheet/);
  for (const kind of ['comment', 'keyword', 'string', 'number', 'variable']) {
    assert.match(portalHTML, new RegExp(`\\.syntax-${kind}\\s*\\{[^}]*var\\(--`));
  }
});

const calendarFixture = year => {
  const days = [];
  for (let day = new Date(Date.UTC(year, 0, 1)); day.getUTCFullYear() === year; day.setUTCDate(day.getUTCDate() + 1)) {
    const date = day.toISOString().slice(0, 10);
    const future = date > `${year}-03-01`;
    days.push({date, total_tokens: future ? null : 1000, requests: future ? null : 2});
  }
  return {stats: {request_count: 2, prompt_tokens: 400, completion_tokens: 600, total_tokens: 1000}, calendar: {year, timezone: 'Asia/Taipei', days}};
};

for (const year of [2023, 2024, 2025, 2028]) {
  test(`Taipei calendar alignment, leap days and future blanks: ${year}`, () => {
    const {context, elements, run} = page(portalHTML);
    context.data = calendarFixture(year);
    const days = context.data.calendar.days;
    run('renderUsage(data)');
    const cells = elements.get('calendar').children;
    const offset = new Date(Date.UTC(year, 0, 1)).getUTCDay();
    assert.equal(cells.length, offset + days.length);
    assert.equal(days.length, year % 4 === 0 ? 366 : 365);
    assert.equal(elements.get('calendar-title').textContent, `${year} · Asia/Taipei (UTC+8) calendar year`);
    assert.equal(elements.get('usage').hidden, false);
    for (let i = 0; i < offset; i++) assert.equal(cells[i].attributes['aria-hidden'], 'true');
    days.forEach((day, index) => {
      const cell = cells[offset + index];
      assert.equal(cell.type, 'button');
      const label = cell.attributes['aria-label'];
      assert.ok(label.startsWith(day.date + ' Asia/Taipei (UTC+8)'));
      if (day.total_tokens === null) {
        assert.equal(cell.disabled, true);
        assert.equal(cell.classes.length, 0);
        assert.equal(label, cell.title);
        assert.ok(!label.includes('0 tokens'));
      } else {
        assert.notEqual(cell.disabled, true);
        assert.ok(label.includes('1,000 total tokens, 2 requests'));
        assert.ok(cell.classes.includes('level2'));
        // Keyboard focus opens the same tooltip hover does; blur closes it.
        cell.onfocus();
        assert.equal(elements.get('day-tip').hidden, false);
        assert.equal(elements.get('day-tip-title').textContent, day.date + ' Asia/Taipei (UTC+8)');
        assert.match(elements.get('day-tip-rows').textContent, /Requests2Total tokens1,000/);
        cell.onblur();
        assert.equal(elements.get('day-tip').hidden, true);
      }
    });
    assert.equal(elements.get('months').children.length, 12);
    elements.get('months').children.forEach((label, month) => {
      const dayIndex = days.findIndex(day => day.date === `${year}-${String(month + 1).padStart(2, '0')}-01`);
      assert.equal(label.style.gridColumn, String(Math.floor((offset + dayIndex) / 7) + 1));
    });
  });
}

test('fixed color bins and legend share CSS classes in both themes', () => {
  const {run} = page(portalHTML);
  for (const [tokens, level] of [[0, 0], [1, 1], [999, 1], [1000, 2], [9999, 2], [10000, 3], [99999, 3], [100000, 4]]) {
    assert.equal(run(`tokenLevel(${tokens})`), level);
    if (level) {
      // The level sets --fill; the cell paints --fill so the button hover rule cannot repaint it.
      assert.match(portalHTML, new RegExp(`\\.level${level}\\s*\\{\\s*--fill:\\s*var\\(--lvl${level}\\)`));
      assert.ok(portalHTML.includes(`class="swatch level${level}"`));
      // Each level token is defined for light, system-dark and forced-dark.
      assert.equal((portalHTML.match(new RegExp(`--lvl${level}:`, 'g')) || []).length, 3);
    }
  }
  assert.ok(portalHTML.includes('1–999'));
  assert.ok(portalHTML.includes('1,000–9,999'));
  assert.ok(portalHTML.includes('10,000–99,999'));
  assert.ok(portalHTML.includes('100,000+'));
  assert.ok(portalHTML.includes('.day:focus-visible'));
});

test('policy renders models as a list, never raw JSON, and follows key state', () => {
  const {context, elements, run} = page(portalHTML);
  const usage = calendarFixture(2025);
  context.data = {...usage, state: 'unissued'};
  run('renderPolicy(data)');
  assert.equal(elements.get('create').hidden, false);
  assert.equal(elements.get('rotate').hidden, true);
  assert.equal(elements.get('policy-ledger').hidden, true);
  assert.equal(elements.get('key-state').textContent, 'not created');
  assert.equal(elements.get('models-empty').hidden, false);

  context.data = {...usage, state: 'active', binding: {key_id: 'key_1', revision: 3},
    key: {models: ['gpt-a', 'claude-b'], max_budget: null, budget_duration: '', expires_at: null, disabled: false}};
  run('renderPolicy(data)');
  assert.equal(elements.get('rotate').hidden, false);
  assert.equal(elements.get('create').hidden, true);
  assert.equal(elements.get('policy-ledger').hidden, false);
  assert.deepEqual(elements.get('models').children.map(li => li.textContent), ['gpt-a', 'claude-b']);
  assert.equal(elements.get('models-empty').hidden, true);
  assert.equal(elements.get('policy-budget').textContent, 'None');
  assert.equal(elements.get('policy-expiry').textContent, 'None');
  assert.equal(elements.get('policy-status').textContent, 'Active');
  assert.equal(elements.get('policy-keyid').textContent, 'key_1');
  for (const id of ['key-summary', 'policy-budget', 'models-empty']) assert.ok(!elements.get(id).textContent.includes('{'));

  context.data = {...usage, state: 'active', binding: {key_id: 'key_1', revision: 3},
    key: {models: [], max_budget: 12.5, budget_duration: '720h', expires_at: '2027-01-02T03:04:05Z', disabled: false}};
  run('renderPolicy(data)');
  assert.equal(elements.get('models').children.length, 0);
  assert.equal(elements.get('models-empty').hidden, false);
  assert.match(elements.get('models-empty').textContent, /No models authorized/);
  assert.equal(elements.get('policy-budget').textContent, 'USD 12.5 per 720h');
  assert.equal(elements.get('policy-expiry').textContent, '2027-01-02 11:04 Asia/Taipei (UTC+8)');

  context.data.key.disabled = true;
  run('renderPolicy(data)');
  assert.equal(elements.get('key-state').textContent, 'disabled');
  assert.equal(elements.get('policy-status').textContent, 'Disabled');

  context.data = {...usage, state: 'revoked', binding: {key_id: 'key_1', revision: 3}};
  run('renderPolicy(data)');
  assert.equal(elements.get('create').hidden, true);
  assert.equal(elements.get('rotate').hidden, true);
  assert.equal(elements.get('key-state').textContent, 'revoked');
});

test('client examples follow the authorized model list and the research-verified bases', () => {
  const {context, elements, snippet, blocks, run} = page(portalHTML);
  assert.ok(blocks.length >= 12, 'every tab has snippet blocks');
  context.models = [];
  run('renderModelPicker(models)');
  const select = elements.get('model-select');
  assert.equal(select.disabled, true);
  assert.equal(select.children[0].textContent, 'Authorization required');
  assert.equal(snippet('env'), "export AGENT_MODEL_API_KEY='YOUR_GATEWAY_KEY'");
  // The model is written into every example; only the key comes from the environment.
  assert.ok(snippet('claude-launch').includes("claude --model 'YOUR_AUTHORIZED_MODEL_ID'"));
  assert.ok(snippet('codex-launch').includes("--model 'YOUR_AUTHORIZED_MODEL_ID'"));
  assert.equal(snippet('pi-launch'), "pi --model 'agent-model/YOUR_AUTHORIZED_MODEL_ID'");
  assert.deepEqual(JSON.parse(snippet('pi-json')).providers['agent-model'].models.map(m => m.id), ['YOUR_AUTHORIZED_MODEL_ID']);

  context.models = ['gpt-a', 'claude-b'];
  run('renderModelPicker(models)');
  assert.equal(select.disabled, false);
  assert.equal(select.value, 'gpt-a');
  assert.ok(snippet('claude-launch').includes("claude --model 'gpt-a'"));
  const settings = JSON.parse(snippet('claude-settings'));
  assert.deepEqual(settings, {env: {ANTHROPIC_BASE_URL: 'https://portal.example.com', ANTHROPIC_AUTH_TOKEN: 'YOUR_GATEWAY_KEY'}, model: 'gpt-a'});
  assert.equal(snippet('claude-run'), 'claude');
  assert.ok(snippet('pi-launch').includes("agent-model/gpt-a'"));
  for (const name of ['ts-code', 'py-code', 'go-code', 'rs-code']) assert.ok(snippet(name).includes('"gpt-a"'), name);
  // pi: one provider entry per authorized model, secret only as an $ENV reference.
  const pi = JSON.parse(snippet('pi-json')).providers['agent-model'];
  assert.equal(pi.apiKey, '$AGENT_MODEL_API_KEY');
  assert.equal(pi.baseUrl, 'https://portal.example.com/v1');
  assert.equal(pi.api, 'openai-completions');
  assert.deepEqual(pi.models.map(m => m.id), ['gpt-a', 'claude-b']);

  // Claude Code: host only, Bearer via ANTHROPIC_AUTH_TOKEN, x-api-key path unset.
  const claude = snippet('claude-launch');
  assert.ok(claude.includes("export ANTHROPIC_BASE_URL='https://portal.example.com'"));
  assert.ok(!claude.includes('portal.example.com/v1'));
  assert.ok(claude.includes('export ANTHROPIC_AUTH_TOKEN="$AGENT_MODEL_API_KEY"'));
  assert.ok(claude.includes('unset ANTHROPIC_API_KEY'));
  // OpenAI SDKs and Codex: /v1 base, responses wire API only.
  // Every SDK example is the official (or, for Rust, the de facto) OpenAI client pointed at /v1.
  for (const name of ['ts-code', 'py-code', 'go-code', 'rs-code']) assert.ok(snippet(name).includes('https://portal.example.com/v1"'), name);
  assert.ok(snippet('go-code').includes('github.com/openai/openai-go/v3'));
  assert.ok(snippet('rs-code').includes('async_openai'));
  const codex = snippet('codex-toml');
  assert.ok(codex.includes('base_url = "https://portal.example.com/v1"'));
  assert.ok(codex.includes('wire_api = "responses"'));
  assert.ok(codex.includes('env_key = "AGENT_MODEL_API_KEY"'));
  assert.ok(!codex.includes('chat'));
  // Only environment-variable references, never a literal credential.
  for (const block of blocks) {
    const text = block.children[0].textContent;
    assert.ok(!/sk-[A-Za-z0-9]/.test(text) && !/Bearer [A-Za-z0-9]/.test(text), block.getAttribute('data-snippet'));
    assert.ok(!text.includes('chatgpt/'), 'no vendor-prefixed default');
  }
  // Unsafe identifiers never reach shell syntax.
  context.models = ["a'b; rm -rf /"];
  run('renderModelPicker(models)');
  for (const block of blocks) assert.ok(!block.children[0].textContent.includes('rm -rf'), block.getAttribute('data-snippet'));
  assert.ok(snippet('claude-launch').includes("'YOUR_AUTHORIZED_MODEL_ID'"));
  assert.equal(run("shellModel('claude-3.5:latest/x', 'P')"), 'claude-3.5:latest/x');
  assert.equal(run("shellModel('', 'P')"), 'P');
});

test('the reported gateway origin replaces the built-in default in every example', async () => {
  const {context, elements, snippet, run} = page(portalHTML);
  const usage = {stats: {request_count: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0}, calendar: {year: 2026, days: [{date: '2026-01-01', total_tokens: null, requests: null}]}};
  context.fetch = async () => ({ok: true, status: 200, json: async () => ({...usage, email: 'a@example.com', csrf_token: 'x', is_admin: false, state: 'active', binding: {key_id: 'k', revision: 1}, key: {models: ['gpt-a']}, gateway_origin: 'https://gw.example.com'})});
  await run('refresh()');
  assert.equal(elements.get('fact-v1').textContent, 'https://gw.example.com/v1');
  assert.equal(elements.get('fact-host').textContent, 'https://gw.example.com');
  assert.ok(snippet('ts-code').includes('https://gw.example.com/v1"'));
  assert.ok(snippet('claude-launch').includes("export ANTHROPIC_BASE_URL='https://gw.example.com'"));
  assert.equal(JSON.parse(snippet('pi-json')).providers['agent-model'].baseUrl, 'https://gw.example.com/v1');
});

test('SDK demos stay focused on the happy-path call and environment credentials', () => {
  const {context, snippet, run} = page(portalHTML);
  context.models = ['demo-model'];
  run('renderModelPicker(models)');
  for (const name of ['ts-code', 'py-code', 'go-code', 'rs-code']) {
    const source = snippet(name);
    assert.match(source, /AGENT_MODEL_API_KEY/);
    assert.match(source, /"demo-model"/);
    assert.doesNotMatch(source, /YOUR_AUTHORIZED|LimitReader|CheckRedirect|Policy::none|Gateway request failed|No text completion|try:|catch\(/);
    assert.match(source, /Say hello in one sentence\./);
  }
  assert.match(snippet('py-code'), /# dependencies = \["openai"\]/);
  assert.equal(snippet('py-run'), 'uv run example.py');
  assert.match(snippet('rs-setup'), /cargo add async-openai --features chat-completion/);
  assert.match(snippet('go-setup'), /go get github.com\/openai\/openai-go\/v3/);
});

test('gateway error codes map to guidance and non-JSON answers are treated as session loss', async () => {
  const {context, run} = page(portalHTML);
  for (const code of ['access_assertion_required', 'csrf_rejected', 'migration_required', 'unsupported_key_policy', 'revision_conflict', 'key_unavailable', 'store_unavailable']) {
    assert.notEqual(run(`explain(${JSON.stringify(code)})`), run("explain('zzz')"));
  }
  assert.match(run("explain('unknown_code')"), /unknown_code/);
  context.fetch = async () => ({ok: true, status: 200, json: async () => { throw new SyntaxError('Unexpected token <'); }});
  await assert.rejects(run("api('/portal/api/me')"), error => error.code === 'session_not_json' && /Reload/.test(error.message));
  context.fetch = async () => ({ok: false, status: 409, json: async () => ({error: 'revision_conflict'})});
  await assert.rejects(run("api('/portal/api/key/rotate', {method: 'POST'})"), error => error.code === 'revision_conflict' && error.status === 409);
});

test('theme override toggles the root attribute and system clears it', () => {
  const {documentElement, run} = page(portalHTML);
  run("applyTheme('dark')");
  assert.equal(documentElement.getAttribute('data-theme'), 'dark');
  run("applyTheme('light')");
  assert.equal(documentElement.getAttribute('data-theme'), 'light');
  run("applyTheme('system')");
  assert.equal(documentElement.getAttribute('data-theme'), null);
});

test('admin overview renders badges, per-key model editors and totals', () => {
  const {context, elements, run} = page(adminHTML);
  context.d = {
    since: '2026-08-09T10:00:00Z', until: '2026-09-08T10:00:00Z',
    stats: {request_count: 1234, prompt_tokens: 5000, completion_tokens: 600, total_tokens: 5600},
    keys: [
      {key_id: 'key_a', name: 'a@example.com', kind: 'portal', state: 'active', models: ['gpt-a', 'old-model'], stats: {request_count: 1000, prompt_tokens: 4000, completion_tokens: 500, total_tokens: 4500}},
      {key_id: 'key_b', name: 'b@example.com', kind: 'portal', state: 'active', models: [], stats: {request_count: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0}},
      {key_id: 'key_c', name: 'c@example.com', kind: 'portal', state: 'revoked', models: [], stats: {request_count: 4, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0}},
      {key_id: 'key_d', name: 'd@example.com', kind: 'portal', state: 'disabled', models: ['gpt-a'], stats: {request_count: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0}},
      {key_id: 'key_e', name: 'legacy-ci', kind: 'legacy_current_hash', state: 'active', models: null, stats: {request_count: 230, prompt_tokens: 1000, completion_tokens: 100, total_tokens: 1100}},
    ],
  };
  context.catalog = ['gpt-a', 'claude-b'];
  run('renderOverview(d, catalog)');
  assert.equal(elements.get('window').textContent, '2026-08-09 18:00 → 2026-09-08 18:00');
  assert.equal(elements.get('count').textContent, '5');
  assert.equal(elements.get('requests').textContent, '1,234');
  assert.equal(elements.get('total').textContent, '5,600');
  const rows = elements.get('keys').querySelectorAll('tr[data-search]').sort((a, b) => a.children[1].textContent.localeCompare(b.children[1].textContent));
  assert.equal(rows.length, 5);
  const badgesOf = row => [row.children[2].children[0], row.children[3].children[0]].map(b => [b.textContent, b.className]);
  assert.deepEqual(badgesOf(rows[0]), [['Portal', 'badge ok'], ['active', 'badge ok']]);
  assert.deepEqual(badgesOf(rows[2])[1], ['revoked', 'badge danger']);
  assert.deepEqual(badgesOf(rows[3])[1], ['disabled', 'badge warn']);
  assert.deepEqual(badgesOf(rows[4]), [['Legacy', 'badge neutral'], ['active', 'badge ok']]);
  assert.equal(rows[0].children[8].textContent, '4,500');
  assert.equal(rows[0].children[8].className, 'num');

  // Editable portal key: chips summary, catalog checkboxes, absent-model note.
  const editor = rows[0].children[4].children[0];
  assert.equal(editor.tag, 'details');
  // The checkbox sheet is built on first open.
  assert.equal(editor.querySelectorAll('input').length, 0);
  editor.open = true; editor.ontoggle();
  assert.deepEqual(editor.children[0].children[0].children.map(li => li.textContent), ['gpt-a', 'old-model']);
  const inputs = editor.querySelectorAll('input');
  assert.deepEqual(inputs.map(i => [i.value, i.checked]), [['gpt-a', true], ['claude-b', false]]);
  assert.match(editor.children[1].textContent, /old-model/);
  assert.equal(editor.querySelectorAll('input:checked').length, 1);
  // Deny-all default and non-editable rows.
  assert.equal(rows[1].children[4].children[0].children[0].children[0].textContent, 'No models · inference denied');
  assert.equal(rows[2].children[4].textContent, 'Revoked — not editable');
  assert.equal(rows[4].children[4].textContent, 'Unmanaged legacy key');

  elements.get('filter').value = 'legacy';
  run('applyFilter()');
  assert.deepEqual(rows.map(r => r.hidden), [true, true, true, true, false]);
  elements.get('filter').value = '';
  run('applyFilter()');
  assert.ok(rows.every(r => !r.hidden));

  context.d.keys = [];
  run('renderOverview(d, catalog)');
  assert.equal(elements.get('keys').children[0].className, 'empty');
});

test('monitoring renders complete totals, zero buckets and unknown details safely', () => {
  const {context, elements, run} = page(adminHTML);
  const stats = {request_count: 100, prompt_tokens: 1000, completion_tokens: 200, total_tokens: 1200, error_count: 2, cache_read_tokens: 50, cache_creation_tokens: 10, reasoning_tokens: 0, reasoning_known_requests: 1, mean_latency_ms: 12.5, priced_cost_usd: .1, priced_requests: 5, subscription_requests: 80, unpriced_requests: 10, unknown_cost_requests: 4, response_cache_requests: 1};
  context.report = {filter: {since: '2026-09-01T00:00:00Z', until: '2026-09-03T00:00:00Z', bucket: 'day', page: 1, page_size: 25}, stats, p95_latency_ms: 20,
    buckets: [{start:'2026-09-01T00:00:00Z',stats:{request_count:0,total_tokens:0}},{start:'2026-09-02T00:00:00Z',stats}],
    models:[{id:'<img src=x>',name:'<img src=x>',stats}],users:[{id:'key_a',name:'alice',stats}],request_count:100,options:{models:['<img src=x>'],providers:['p'],users:[{id:'key_a',name:'alice'}]},
    requests:[{id:'r1',created_at:'2026-09-02T00:00:00Z',user:'alice',key_id:'key_a',model:'<img src=x>',model_used:'m',provider:'p',auth_mode:'api_key',status:'error',prompt_tokens:10,completion_tokens:2,total_tokens:12,cache_read_tokens:4,cache_creation_tokens:1,reasoning_tokens:null,latency_ms:20,cost_class:'unpriced',priced_cost_usd:null}]};
  run('renderMonitoring(report)');
  assert.equal(elements.get('mon-results').hidden, false);
  assert.match(elements.get('mon-page').textContent,/Page 1 of 4 · 100 requests/);
  assert.equal(elements.get('mon-prev').disabled,true);
  assert.equal(elements.get('mon-next').disabled,false);
  assert.equal(elements.get('mon-trend').children.length,3); // Two buckets plus baseline.
  assert.equal(elements.get('mon-trend').children[0].getAttribute('height'),'0');
  assert.match(elements.get('mon-stats').textContent,/0 · 1 of 100 known/);
  assert.match(elements.get('mon-details').textContent,/—/); // Unknown reasoning is not zero.
  assert.match(elements.get('mon-details').textContent,/unpriced/);
  assert.ok(!descendants(elements.get('mon-details')).some(e=>e.tag==='img'));
  context.report.requests[0].reasoning_tokens=0;
  run('renderMonitoring(report)');
  assert.equal(elements.get('mon-details').children[0].children[12].textContent,'0');
  run("$('mon-key').value='key_a'");elements.get('mon-model').value='<img src=x>';
  assert.deepEqual(JSON.parse(run('JSON.stringify(monitorQuery())')), {key_id:'key_a',model:'<img src=x>'});
  run("$('mon-bucket').value='hour'");
  assert.equal(JSON.parse(run('JSON.stringify(monitorQuery())')).bucket, 'hour');
});

test('monitoring paging freezes the returned range and failed loads hide stale results', async () => {
  const {context,elements,run}=page(adminHTML);
  const calls=[];
  context.fetch=async path=>{calls.push(path);return {ok:true,status:200,json:async()=>({filter:{since:'2026-09-01T00:00:00Z',until:'2026-09-03T00:00:00Z',bucket:'day',page:calls.length,page_size:25},stats:{request_count:50},p95_latency_ms:null,buckets:[],models:[],users:[],requests:[],options:{models:[],providers:[],users:[]}})};};
  run("$('mon-key').value='key_a'; monitorFilter = monitorQuery()");
  await run('loadMonitoring()');
  run("monitorPage=2; $('mon-key').value='unsaved-selection'");
  await run('loadMonitoring()');
  const query=new URL(calls[1],'https://portal.example.com').searchParams;
  assert.equal(query.get('key_id'),'key_a');
  assert.equal(query.get('since'),'2026-09-01T00:00:00Z');
  assert.equal(query.get('until'),'2026-09-03T00:00:00Z');
  assert.equal(query.get('page'),'2');
  context.fetch=async()=>({ok:false,status:503,json:async()=>({error:'store_unavailable'})});
  await run('loadMonitoring()');
  assert.equal(elements.get('mon-results').hidden,true);
  assert.match(elements.get('mon-message').textContent,/store is unavailable/);
  assert.equal(elements.get('mon-apply').disabled,false);
});

test('admin state control preserves the adjacent unsaved model editor', async () => {
  const {context,run} = page(adminHTML);
  context.key={key_id:'a',name:'Alice',kind:'portal',state:'active',models:['m'],stats:{}};
  context.row=run('renderRow(key,["m"])');
  const editor=context.row.children[4];
  const calls=[];
  context.fetch=async (path,init)=>{calls.push([path,init]);return {ok:true,status:200,json:async()=>({key_id:'a',disabled:true})};};
  await context.row.children[3].children[2].onclick();
  assert.equal(context.row.children[4],editor);
  assert.equal(context.row.children[3].children[2].textContent,'Enable');
  assert.equal(calls[0][0],'/portal/admin/api/state');
  assert.deepEqual(JSON.parse(calls[0][1].body),{key_id:'a',disabled:true});
  assert.equal(run('stateControls({...key,kind:"legacy_current_hash"}).children.length'),1);
  assert.equal(run('stateControls({...key,state:"revoked"}).children.length'),1);
});

function monitoringFixture() {
  return {filter:{since:'2026-09-01T00:00:00Z',until:'2026-09-03T00:00:00Z',bucket:'day',page:1,page_size:25},request_count:50,stats:{request_count:50,error_count:5},p95_latency_ms:null,buckets:[],models:[],users:[],requests:[],options:{models:[],providers:[],users:[]}};
}

test('admin routes default and invalid tabs to Overview, support direct Requests and back history', async () => {
  for (const search of ['', '?tab=invalid', '?tab=overview', '?tab=users', '?tab=requests&model=a%2Fb%26c']) {
    const {context,elements,location,history,run} = page(adminHTML, search);
    const expected = search.includes('tab=users') ? 'users' : search.includes('tab=requests') ? 'requests' : 'overview';
    assert.equal(run('activeTab'), expected);
    assert.equal(elements.get('nav-' + expected).getAttribute('aria-current'), 'page');
    assert.equal(elements.get('request-metadata').hidden, expected !== 'requests');
    assert.equal(elements.get('users-panel').hidden, expected !== 'users');
    context.fetch = async () => ({ok:true,status:200,json:async()=>monitoringFixture()});
    await elements.get('nav-requests').onclick({preventDefault(){}});
    assert.equal(new URLSearchParams(location.search).get('tab'),'requests');
    await history.back();
    assert.equal(run('activeTab'),expected);
  }
});

test('Users loads only keys/catalog once and preserves unsaved editors across tabs', async () => {
  const {context,elements,run} = page(adminHTML,'?tab=users');
  const calls=[];
  context.fetch=async path=>{
    calls.push(path);
    const data=path.endsWith('/models') ? {catalog:['m']} : path.endsWith('/overview') ? {since:'2026-09-01',until:'2026-09-03',stats:{},keys:[{key_id:'key_a',name:'Alice',kind:'portal',state:'active',models:['m'],stats:{}}]} : monitoringFixture();
    return {ok:true,status:200,json:async()=>data};
  };
  await run('signedIn=true; showRoute()');
  assert.deepEqual(calls,['/portal/admin/api/overview','/portal/admin/api/models']);
  const row=elements.get('keys').querySelectorAll('tr[data-search]')[0];
  const editor=row.children[4];
  // Open the editor so its checkbox sheet exists, as an administrator would.
  const details=editor.children[0]; details.open=true; details.ontoggle();
  const input=editor.querySelector('input'); input.checked=false;
  await elements.get('nav-overview').onclick();
  assert.equal(elements.get('mon-summary').children.length,5);
  assert.match(elements.get('mon-summary').textContent,/90.0%/);
  assert.equal(elements.get('request-metadata').hidden,true);
  await elements.get('nav-users').onclick();
  assert.equal(elements.get('keys').querySelectorAll('tr[data-search]')[0],row);
  assert.equal(row.children[4],editor);
  assert.equal(input.checked,false);
  assert.equal(calls.filter(p=>p.endsWith('/overview')).length,1);
});

test('retained Users drilldowns use newly applied filters for clicks and new-tab hrefs without rebuilding editors', async () => {
  const rangeA={since:'2026-08-01T00:00:00Z',until:'2026-08-03T00:00:00Z'};
  const rangeB={since:'2026-09-01T00:00:00Z',until:'2026-09-03T00:00:00Z'};
  const model='a/b & <model>', keyID='key_+&/';
  const {context,elements,location,run}=page(adminHTML,'?' + new URLSearchParams({tab:'users',...rangeA}));
  const calls=[];
  context.fetch=async path=>{
    calls.push(path);
    const query=new URL(path,'https://portal.example.com').searchParams;
    const report=monitoringFixture();
    report.filter={...report.filter,...Object.fromEntries(query)};
    const data=path.endsWith('/models') ? {catalog:['m',model]} : path.endsWith('/overview') ? { ...rangeA,stats:{},keys:[{key_id:keyID,name:'Alice',kind:'portal',state:'active',models:['m'],stats:{}}]} : report;
    return {ok:true,status:200,json:async()=>data};
  };
  await run('signedIn=true; showRoute()');
  const row=elements.get('keys').querySelectorAll('tr[data-search]')[0];
  const link=row.children[0].children.find(n=>n.tag==='a');
  const details=row.children[4].children[0];
  details.open=true; details.ontoggle();
  const input=details.querySelector('input'); input.checked=false;
  assert.equal(new URLSearchParams(link.href).get('since'),rangeA.since);

  await elements.get('nav-overview').onclick();
  elements.get('mon-since').value=run(`taipeiInput('${rangeB.since}')`);
  elements.get('mon-until').value=run(`taipeiInput('${rangeB.until}')`);
  elements.get('mon-model').value=model;
  elements.get('mon-status').value='error';
  elements.get('mon-user').value='another-user';
  elements.get('mon-key').value='another-key';
  // Draft controls must not affect drilldowns until Apply.
  assert.equal(new URLSearchParams(link.href).get('since'),rangeA.since);
  await elements.get('mon-apply').onclick();
  await elements.get('nav-users').onclick();
  assert.equal(elements.get('keys').querySelectorAll('tr[data-search]')[0],row);
  assert.equal(row.children[4].children[0],details);
  assert.equal(details.open,true);
  assert.equal(details.querySelector('input'),input);
  assert.equal(input.checked,false);
  assert.equal(calls.filter(p=>p.endsWith('/overview')).length,1);
  assert.equal(calls.filter(p=>p.endsWith('/models')).length,1);

  const expected={tab:'requests',...rangeB,key_id:keyID,model,status:'error'};
  assert.deepEqual(Object.fromEntries(new URLSearchParams(link.href)),expected);
  const usersURL=location.search, callCount=calls.length;
  for (const modifier of [{ctrlKey:true},{metaKey:true},{shiftKey:true},{button:1}]) {
    await link.onclick({...modifier,preventDefault(){assert.fail('modified clicks must remain native');}});
    assert.equal(location.search,usersURL);
    assert.equal(calls.length,callCount);
  }
  // Opening the href directly (including a context-menu new tab) carries the same filters.
  const newTab=page(adminHTML,link.href);
  assert.equal(newTab.run('activeTab'),'requests');
  const {tab,...filters}=expected;
  assert.deepEqual(JSON.parse(newTab.run('JSON.stringify(monitorFilter)')),filters);
  let prevented=false;
  await link.onclick({preventDefault(){prevented=true;}});
  assert.equal(prevented,true);
  assert.deepEqual(Object.fromEntries(new URLSearchParams(location.search)),expected);
  assert.deepEqual(Object.fromEntries(new URL(calls.at(-2),'https://portal.example.com').searchParams),{...filters,page:'1',view:'requests',timezone:'Asia/Taipei'});
});

test('breakdown and user drilldowns encode exact filters and preserve frozen range on refresh', async () => {
  const {context,elements,location,run}=page(adminHTML);
  const calls=[];
  context.fetch=async path=>{calls.push(path);return {ok:true,status:200,json:async()=>monitoringFixture()};};
  await run('signedIn=true; showRoute()');
  context.report=monitoringFixture();
  context.report.models=[{id:'a/b & <model>',name:'a/b & <model>',stats:{}}];
  context.report.users=[{id:'key_+&/',name:'Alice',stats:{}}];
  run('renderMonitoring(report)');
  const link=elements.get('mon-models').children[0].children[0].children[0];
  await link.onclick();
  assert.equal(new URLSearchParams(location.search).get('model'),'a/b & <model>');
  const userLink=run('requestLink("View requests", "key_id", "key_+&/")');
  await userLink.onclick();
  const query=new URLSearchParams(location.search);
  assert.equal(query.get('key_id'),'key_+&/');
  assert.equal(query.get('tab'),'requests');
  assert.equal(query.get('since'),'2026-09-01T00:00:00Z');
  assert.equal(page(adminHTML,location.search).run('monitorFilter.key_id'),'key_+&/');
  assert.equal(new URL(calls.at(-1),'https://portal.example.com').searchParams.get('key_id'),'key_+&/');
});

test('URL filter validation stays server-owned and obsolete loads cannot rewrite a newer route', async () => {
  const {context,elements,location,run}=page(adminHTML,'?tab=requests&since=bad&page=oops&status=invalid');
  let request;
  context.fetch=async path=>{request=path;return {ok:false,status:400,json:async()=>({error:'invalid_monitoring_filter'})};};
  await run('signedIn=true; showRoute()');
  const query=new URL(request,'https://portal.example.com').searchParams;
  assert.equal(query.get('since'),'bad'); assert.equal(query.get('page'),'oops'); assert.equal(query.get('status'),'invalid');
  assert.equal(elements.get('mon-results').hidden,true);
  assert.match(elements.get('mon-message').textContent,/Check the Taipei \(UTC\+8\) dates/);
  let resolve;
  context.fetch=()=>new Promise(r=>{resolve=r;});
  const pending=run('loadMonitoring()');
  run('signedIn=false');
  await elements.get('nav-users').onclick();
  resolve({ok:true,status:200,json:async()=>monitoringFixture()});
  await pending;
  assert.equal(new URLSearchParams(location.search).get('tab'),'users');
  assert.equal(elements.get('monitor-panel').hidden,true);
});

test('pages respect the nonce CSP and stay self-contained', () => {
  for (const [name, html] of [['portal.html', portalHTML], ['portal_admin.html', adminHTML]]) {
    assert.ok(!/\sstyle="/.test(html), `${name}: inline style attributes are blocked by style-src nonce`);
    assert.ok(!/<(link|img|iframe|form)\b/i.test(html), `${name}: no external resources or forms`);
    assert.ok(!/\b(src|href)="https?:/.test(html), `${name}: no remote references`);
    assert.equal((html.match(/<script /g) || []).length, 1, `${name}: single nonce script`);
    assert.equal((html.match(/<style /g) || []).length, 1, `${name}: single nonce style`);
    assert.ok(html.includes('[hidden] { display: none !important; }'), `${name}: hidden survives display overrides`);
    assert.ok(html.includes('prefers-color-scheme: dark') && html.includes(':root[data-theme="dark"]'), `${name}: theme tokens`);
    assert.ok(html.includes('[data-theme-choice]'), `${name}: theme toggle`);
    assert.ok(!/localStorage\.setItem\((?!THEME_KEY)/.test(html), `${name}: only the theme is persisted`);
    assert.ok(!/sessionStorage|indexedDB|document\.cookie/.test(html), `${name}: no other browser storage`);
    assert.ok(html.includes("'X-CSRF-Token': csrf"), `${name}: CSRF header preserved`);
  }
  assert.ok(portalHTML.includes('<pre id="token"></pre>'), 'secret is never placed in a form field');
  assert.ok(portalHTML.includes("window.addEventListener('pagehide', clearSecret)"));
});

test('Overview projects summary; Requests loads options once and pagination only fetches rows/count', async () => {
  const {context,elements,run}=page(adminHTML);
  const calls=[];
  context.fetch=async path=>{
    const q=new URL(path,'https://portal.example.com').searchParams;
    calls.push(q);
    const full=monitoringFixture();
    full.filter.page=Number(q.get('page') || 1);
    full.options.users=[{id:'key_a',name:'Alice'}];
    const {filter,options}=full;
    const data=q.get('view')==='requests' ? {filter,request_count:50,requests:[]} : q.get('view')==='options' ? {filter,options} : {...full,requests:undefined};
    return {ok:true,status:200,json:async()=>data};
  };
  await run('signedIn=true; showRoute()');
  assert.deepEqual(calls.map(q=>q.get('view')),['summary']);
  assert.equal(elements.get('mon-results').hidden,false);
  await elements.get('nav-requests').onclick();
  assert.deepEqual(calls.map(q=>q.get('view')),['summary','requests']);
  await elements.get('mon-next').onclick();
  assert.deepEqual(calls.map(q=>q.get('view')),['summary','requests','requests']);
  assert.equal(calls.at(-1).get('page'),'2');
  assert.equal(calls.at(-1).get('since'),'2026-09-01T00:00:00Z');
  assert.match(elements.get('mon-page').textContent,/Page 2 of 2 · 50 requests/);

  const direct=page(adminHTML,'?tab=requests&model=m');
  direct.context.fetch=context.fetch;
  calls.length=0;
  await direct.run('signedIn=true; showRoute()');
  assert.deepEqual(calls.map(q=>q.get('view')),['requests','options']);
  assert.equal(calls[1].get('since'),'2026-09-01T00:00:00Z');
  assert.equal(direct.elements.get('mon-user-options').children[0].value,'Alice');
  await direct.elements.get('mon-next').onclick();
  assert.deepEqual(calls.map(q=>q.get('view')),['requests','options','requests']);
});

test('obsolete selector responses cannot rewrite the route or replace current choices; Apply refreshes options', async () => {
  const {context,elements,location,run}=page(adminHTML,'?tab=requests');
  let resolveOptions;
  const calls=[];
  context.fetch=async path=>{
    const view=new URL(path,'https://portal.example.com').searchParams.get('view');
    calls.push(view);
    if(view==='options') return new Promise(resolve=>{resolveOptions=resolve;});
    const full=monitoringFixture();
    return {ok:true,status:200,json:async()=>({filter:full.filter,request_count:50,requests:[]})};
  };
  const pending=run('signedIn=true; showRoute()');
  // Wait until the row response has scheduled its selector read.
  while(!resolveOptions) await new Promise(resolve=>setImmediate(resolve));
  run('signedIn=false');
  await elements.get('nav-users').onclick();
  resolveOptions({ok:true,status:200,json:async()=>({options:{models:['obsolete'],providers:[],users:[]}})});
  await pending;
  assert.equal(new URLSearchParams(location.search).get('tab'),'users');
  assert.equal(run('monitorOptionValues'),null);
  assert.equal(elements.get('mon-results').hidden,true);

  context.fetch=async path=>{
    calls.push(new URL(path,'https://portal.example.com').searchParams.get('view'));
    const full=monitoringFixture();
    return {ok:true,status:200,json:async()=>({filter:full.filter,request_count:50,requests:[],options:full.options})};
  };
  run('signedIn=true');
  await elements.get('nav-requests').onclick();
  calls.length=0;
  await elements.get('mon-apply').onclick();
  assert.deepEqual(calls,['requests','options']);
});

test('Taipei display and datetime controls are independent of browser timezone', () => {
  const {context, elements, run} = page(adminHTML, '?tab=requests&since=2024-12-31T15%3A59%3A59Z&until=2024-12-31T16%3A00%3A00Z');
  assert.equal(elements.get('mon-since').value, '2024-12-31T23:59:59');
  assert.equal(elements.get('mon-until').value, '2025-01-01T00:00:00');
  assert.equal(run("stamp('2024-12-31T16:00:00Z')"), '2025-01-01 00:00');
  assert.equal(run('monitorQuery().since'), '2024-12-31T15:59:59Z');
  assert.equal(run('monitorQuery().until'), '2024-12-31T16:00:00Z');
  context.bucket = {start: '2024-12-31T16:00:00Z', stats: {request_count: 0}};
  run("showTip($('mon-trend'), bucket, 'day')");
  assert.equal(elements.get('mon-tip-title').textContent, '2025-01-01 Asia/Taipei (UTC+8)');
  run("showTip($('mon-trend'), bucket, 'hour')");
  assert.equal(elements.get('mon-tip-title').textContent, '2025-01-01 00:00 Asia/Taipei (UTC+8)');
  assert.equal(run("taipeiInput('2024-12-31T23:59:59+08:00')"), '2024-12-31T23:59:59');
  assert.equal(run("taipeiInstant('2024-03-10T02:30')"), '2024-03-09T18:30:00Z'); // US DST gap is irrelevant.
  assert.equal(run("taipeiInstant('2024-11-03T01:30')"), '2024-11-02T17:30:00Z'); // US repeated hour is unambiguous.
  const personal = page(portalHTML);
  assert.equal(personal.run("describeExpiry({expires_at:'2024-12-31T16:00:00Z'})"), '2025-01-01 00:00 Asia/Taipei (UTC+8)');
});

test('Taipei Apply, paging, refresh and Back preserve canonical instants and request the reporting zone', async () => {
  const original = {since:'2024-12-31T15:59:59Z', until:'2024-12-31T16:00:00Z'};
  const {context, elements, location, history, run} = page(adminHTML, '?' + new URLSearchParams({tab:'requests', ...original}));
  const calls = [];
  context.fetch = async path => {
    const query = new URL(path, 'https://portal.example.com').searchParams;
    calls.push(query);
    return {ok:true,status:200,json:async()=>({...monitoringFixture(), filter:{...monitoringFixture().filter,...Object.fromEntries(query)}, request_count:50})};
  };
  await run('signedIn=true; showRoute()');
  elements.get('mon-since').value = '2024-02-29T23:59:59';
  elements.get('mon-until').value = '2024-03-01T00:00:00';
  await elements.get('mon-apply').onclick();
  const applied = {since:'2024-02-29T15:59:59Z', until:'2024-02-29T16:00:00Z'};
  await elements.get('mon-next').onclick();
  for (const [name, value] of Object.entries(applied)) {
    assert.equal(calls.at(-1).get(name), value);
    assert.equal(new URLSearchParams(location.search).get(name), value);
  }
  assert.equal(calls.at(-1).get('page'), '2');
  const refreshed = page(adminHTML, location.search);
  assert.equal(refreshed.elements.get('mon-since').value, '2024-02-29T23:59:59');
  assert.equal(refreshed.elements.get('mon-until').value, '2024-03-01T00:00:00');
  await history.back(); // page 1 of the applied range
  await history.back(); // original range
  for (const [name, value] of Object.entries(original)) assert.equal(new URLSearchParams(location.search).get(name), value);
  assert.equal(elements.get('mon-until').value, '2025-01-01T00:00:00');
  assert.ok(calls.every(q => q.get('timezone') === 'Asia/Taipei'));
  assert.ok(calls.some(q => q.get('view') === 'options'));
  const personal = page(portalHTML);
  let personalPath;
  personal.context.fetch = async path => { personalPath = path; return {ok:true,status:200,json:async()=>({...calendarFixture(2025),state:'unissued'})}; };
  await personal.run('refresh()');
  assert.equal(new URL(personalPath, 'https://portal.example.com').searchParams.get('timezone'), 'Asia/Taipei');
});

test('request rows, ranges and accessible labels use Taipei rather than browser wall time', () => {
  const {context,elements,run} = page(adminHTML);
  context.report = {...monitoringFixture(), requests:[{id:'r',created_at:'2024-12-31T16:00:00Z',reasoning_tokens:null,priced_cost_usd:null}]};
  run('renderMonitoring(report)');
  assert.equal(elements.get('mon-details').children[0].children[0].textContent, '2025-01-0100:00');
  assert.equal(elements.get('mon-range').textContent, '2026-09-01 08:00 → 2026-09-03 08:00 Asia/Taipei (UTC+8)');
  for (const label of ['Since, Asia/Taipei UTC+8, inclusive', 'Until, Asia/Taipei UTC+8, exclusive', 'Requests per bucket, Asia/Taipei UTC+8']) assert.ok(adminHTML.includes('aria-label="' + label + '"'));
  for (const html of [portalHTML, adminHTML]) assert.doesNotMatch(html.replace(/\/\*[\s\S]*?\*\/|\/\/[^\n]*/g, ''), /\(UTC\)| UTC[: —]|UTC calendar|UTC day|UTC hour/);
});

test('service creation reveals once, copies safely, appends without discarding editors, and never persists secrets', async () => {
  const {context, elements, listeners, location, run} = page(adminHTML, '?tab=users');
  const writes = [], calls = [], copied = [];
  context.localStorage.setItem = (...args) => writes.push(args);
  context.navigator.clipboard.writeText = async text => copied.push(text);
  const secret = 'sk-service-<img src=x onerror=alert(1)>';
  context.fetch = async (path, init) => {
    calls.push([path, init]);
    return {ok: true, json: async () => ({key_id: 'service-id', name: '<b>Worker</b>', kind: 'service', state: 'active', models: [], stats: {}, key: secret})};
  };
  run(`signedIn = true; usersLoaded = true; csrf = 'csrf'; renderOverview({since:'2026-01-01',until:'2026-02-01',stats:{},keys:[{key_id:'employee',name:'Employee',kind:'portal',state:'active',models:[],stats:{}}]}, ['chatgpt']);`);
  const originalRow = elements.get('keys').querySelectorAll('tr[data-search]')[0];
  const editor = descendants(originalRow).find(n => n.tag === 'details' && n.className === 'editor');
  editor.open = true; editor.ontoggle();
  descendants(editor).find(n => n.tag === 'input').checked = true;
  run(`$('service-name').value = 'Worker'`);
  await elements.get('service-create').onclick();
  assert.equal(calls.length, 1);
  assert.equal(calls[0][0], '/portal/admin/api/service-keys');
  assert.deepEqual(JSON.parse(calls[0][1].body), {name:'Worker'});
  assert.equal(calls[0][1].headers['X-CSRF-Token'], 'csrf');
  assert.equal(elements.get('service-token').textContent, secret);
  assert.equal(elements.get('service-secret').hidden, false);
  assert.equal(elements.get('keys').querySelectorAll('tr[data-search]')[0], originalRow);
  assert.equal(descendants(editor).find(n => n.tag === 'input').checked, true);
  const row = elements.get('keys').querySelectorAll('tr[data-search]')[1];
  assert.match(row.textContent, /Service/);
  assert.match(row.textContent, /No models · inference denied/);
  assert.match(row.textContent, /Disable/);
  assert.doesNotMatch(row.textContent, /sk-service/);
  assert.ok(descendants(row).some(n => n.tag === 'details'));
  assert.ok(!descendants(row).some(n => n.tag === 'b' || n.tag === 'img'));
  await elements.get('service-copy').onclick();
  assert.deepEqual(copied, [secret]);
  elements.get('service-dismiss').onclick();
  assert.equal(elements.get('service-token').textContent, '');
  assert.equal(elements.get('service-secret').hidden, true);
  run(`$('service-token').textContent='another-secret'; $('service-secret').hidden=false`);
  listeners.get('pagehide')();
  assert.equal(elements.get('service-token').textContent, '');
  assert.deepEqual(writes, []);
  assert.equal(location.search, '?tab=users');
});

test('service failed creates do not retry, and navigation cannot resurrect a pending secret', async () => {
  const {context, elements, listeners, run} = page(adminHTML, '?tab=users');
  run(`signedIn=true; usersLoaded=true; usersCatalog=[];`);
  let calls = 0;
  context.fetch = async () => { calls++; return {ok:false,status:409,json:async()=>({error:'service_name_conflict'})}; };
  run(`$('service-name').value = 'Worker'`);
  await elements.get('service-create').onclick();
  assert.equal(calls, 1);
  assert.match(elements.get('status').textContent, /already in use/);
  assert.match(elements.get('status').textContent, /Refresh before trying again/);
  assert.equal(elements.get('service-token').textContent, '');
  let resolve;
  context.fetch = () => { calls++; return new Promise(r => { resolve=r; }); };
  const pending = elements.get('service-create').onclick();
  await elements.get('service-create').onclick();
  assert.equal(calls, 2); // double-click is not a second request
  listeners.get('pagehide')();
  resolve({ok:true,json:async()=>({key:'late-secret',key_id:'late',name:'Worker',kind:'service',state:'active',models:[],stats:{}})});
  await pending;
  assert.equal(elements.get('service-token').textContent, '');
  assert.equal(elements.get('service-secret').hidden, true);
  assert.equal(calls, 2);
});

test('Master key is labeled, read-only, and drills down by reserved identity', () => {
  const {context, run, elements} = page(adminHTML);
  context.key = {key_id: 'system:master', name: 'Master key', kind: 'master', state: 'active', models: null, stats: {request_count: 2}};
  const row = run('renderRow(key, ["m"])');
  assert.match(row.textContent, /Master key/);
  assert.match(row.textContent, /Read-only/);
  assert.equal(descendants(row).filter(n => ['button','input','details'].includes(n.tag)).length, 0);
  const link = descendants(row).find(n => n.tag === 'a');
  assert.equal(new URLSearchParams(link.href).get('key_id'), 'system:master');
  run('renderMonitorOptions({users:[{id:"system:master",name:"Master key"},{id:"service",name:"Master key"}],models:[],providers:[]})');
  const option = elements.get('mon-user-options').children.find(n => n.value === 'system:master');
  assert.equal(option.textContent, 'Master key');
  elements.get('mon-user').value = 'system:master';
  const query = run('monitorQuery()');
  assert.equal(query.key_id, 'system:master');
  assert.equal(query.user, undefined);
  context.report = monitoringFixture();
  context.report.users = [{id:'system:master',name:'Master key',stats:{}},{id:'service',name:'Master key',stats:{}}];
  run('renderMonitoring(report)');
  const rows = elements.get('mon-users').children;
  assert.equal(descendants(rows[0]).filter(n => n.className === 'badge warn').length, 1);
  assert.equal(descendants(rows[1]).filter(n => n.className === 'badge warn').length, 0);
  const masterLink = descendants(rows[0]).find(n => n.tag === 'a');
  assert.equal(new URLSearchParams(masterLink.href).get('key_id'), 'system:master');
});

test('disabled is reversible, revoked is read-only and shows revocation time', () => {
  const {run, context} = page(adminHTML);
  context.key = {key_id: 'key', name: 'User', kind: 'portal', state: 'disabled', models: ['m'], stats: {}};
  assert.match(run('stateControls(key).textContent'), /disabled/);
  assert.doesNotMatch(run('stateControls(key).textContent'), /revoked/);
  context.key.state = 'revoked';
  context.key.revoked_at = '2026-09-10T01:00:00Z';
  const state = run('stateControls(key)');
  assert.equal(descendants(state).filter(n => n.tag === 'button').length, 0);
  assert.match(state.textContent, /2026-09-10/);
  assert.equal(descendants(run('modelControls(key, ["m"])')).filter(n => n.tag === 'details').length, 0);
  assert.match(run('renderRow(key, ["m"]).textContent'), /View requests/);
});

test('inactive groups collapse with counts, search reveals matches and revoke moves a row without editors', async () => {
  const {context, elements, run} = page(adminHTML);
  run(`renderOverview({since:'2026-01-01',until:'2026-02-01',stats:{},keys:[
    {key_id:'active',name:'Active',kind:'service',state:'active',models:['m'],stats:{}},
    {key_id:'paused',name:'Paused',kind:'service',state:'disabled',models:['m'],stats:{}},
    {key_id:'expired',name:'Expired',kind:'service',state:'expired',models:['m'],stats:{}},
    {key_id:'retired',name:'Retired',kind:'service',state:'revoked',revoked_at:'2026-01-02T00:00:00Z',models:['m'],stats:{}}
  ]}, ['m'])`);
  assert.equal(run('keyGroups.get("service:active").group.open'), true);
  assert.equal(run('keyGroups.get("service:inactive").group.open'), false);
  assert.equal(run('keyGroups.get("service:revoked").group.open'), false);
  assert.match(run('keyGroups.get("service:inactive").summary.textContent'), /\(2\)/);
  elements.get('filter').value = 'Retired'; run('applyFilter()');
  assert.equal(run('keyGroups.get("service:revoked").group.open'), true);
  assert.equal(run('keyGroups.get("service:active").group.hidden'), true);
  elements.get('filter').value = ''; run('applyFilter()');
  const row = run('keyGroups.get("service:active").body.children[0]');
  const revoke = descendants(row).find(n => n.tag === 'button' && n.textContent === 'Revoke permanently');
  const calls = []; const confirmations = [];
  context.fetch = async (path, init) => { calls.push([path, init]); return {ok:true,json:async()=>({key_id:'active',state:'revoked',revoked_at:'2026-01-03T00:00:00Z'})}; };
  context.confirm = text => { confirmations.push(text); return false; };
  await revoke.onclick(); assert.equal(calls.length, 0);
  context.confirm = text => { confirmations.push(text); return true; };
  await revoke.onclick();
  assert.match(confirmations[0], /cannot be restored/);
  assert.match(confirmations[0], /Historical requests and costs/);
  assert.equal(calls[0][0], '/portal/admin/api/revoke');
  assert.equal(calls[0][1].method, 'POST');
  assert.deepEqual(JSON.parse(calls[0][1].body), {key_id:'active'});
  assert.equal(run('keyGroups.get("service:active").group.hidden'), true);
  assert.match(run('keyGroups.get("service:revoked").summary.textContent'), /\(2\)/);
  assert.equal(descendants(row.children[3]).filter(n => n.tag === 'button').length, 0);
  assert.equal(descendants(row.children[4]).filter(n => ['input', 'details'].includes(n.tag)).length, 0);
  assert.match(row.textContent, /Management history/);
  assert.match(row.textContent, /Rotate secret/);
  assert.match(row.textContent, /View requests/);
  assert.match(row.textContent, /2026-01-03/);
});

test('personal replacement is explicitly a new secret, not restore or second issuance', () => {
 const {run, elements} = page(portalHTML);
 run(`renderPolicy({state:'revoked',binding:{key_id:'stable',revision:3},key:{models:['old'],disabled:false,revoked_at:'2026-01-01'}})`);
 assert.equal(elements.get('create').hidden, true);
 assert.equal(elements.get('rotate').hidden, false);
 assert.equal(elements.get('rotate').textContent, 'Replace revoked key');
 assert.match(elements.get('key-summary').textContent, /grant models again/);
 assert.equal(elements.get('key-state').textContent, 'revoked');
});

for (const action of ['Disable', 'Revoke permanently']) {
  test(`late ${action} response after Refresh cannot resurrect a detached row`, async () => {
    const {context, elements, run} = page(adminHTML);
    const report = {since:'2026-01-01', until:'2026-02-01', stats:{}, keys:[
      {key_id:'key', name:'Service', kind:'service', state:'active', models:['m'], stats:{}}
    ]};
    context.report = report;
    run('renderOverview(report, ["m"])');
    const oldRow = elements.get('keys').querySelectorAll('tr[data-search]')[0];
    const button = descendants(oldRow).find(n => n.tag === 'button' && n.textContent === action);
    let resolve;
    const delayed = new Promise(done => { resolve = done; });
    context.fetch = async (path, options) => {
      if (options?.method) return delayed;
      return {ok:true, json:async () => path.endsWith('/models') ? {catalog:['m']} : report};
    };
    const pending = button.onclick();
    await elements.get('refresh').onclick();
    const refreshedRow = elements.get('keys').querySelectorAll('tr[data-search]')[0];
    assert.notEqual(refreshedRow, oldRow);
    const status = elements.get('status').textContent;
    resolve({ok:true, json:async () => ({key_id:'key', disabled:true, state:action === 'Disable' ? 'disabled' : 'revoked', revoked_at:'2026-01-02T00:00:00Z'})});
    await pending;
    assert.deepEqual(elements.get('keys').querySelectorAll('tr[data-search]'), [refreshedRow]);
    assert.equal(elements.get('count').textContent, '1');
    assert.equal(run('Array.from(keyGroups.values()).reduce((sum, g) => sum + g.body.children.length, 0)'), 1);
    assert.equal(elements.get('status').textContent, status);
  });
}

test('service rotation reveals once, preserves ID and stats, and sends the observed revision', async () => {
  const {context,elements,run,listeners}=page(adminHTML,'?tab=users');
  context.key={key_id:'svc',name:'Worker',kind:'service',revision:2,state:'revoked',models:['old'],stats:{request_count:7}};
  run(`signedIn=true;usersLoaded=true;csrf='csrf';renderOverview({since:'2026-01-01',until:'2026-02-01',stats:{},keys:[key]},['m'])`);
  const calls=[];context.fetch=async(path,init)=>{calls.push([path,init]);return {ok:true,json:async()=>({key_id:'svc',name:'Worker',kind:'service',revision:3,state:'disabled',models:[],key:'sk-new-<img>'})};};
  let button=descendants(elements.get('keys')).find(n=>n.tag==='button'&&n.textContent==='Rotate secret');
  context.confirm=()=>false;await button.onclick();assert.equal(calls.length,0);
  context.confirm=()=>true;await button.onclick();
  assert.equal(calls.length,1);assert.equal(calls[0][0],'/portal/admin/api/service-keys/rotate');
  assert.deepEqual(JSON.parse(calls[0][1].body),{key_id:'svc',revision:2});
  assert.equal(calls[0][1].headers['X-CSRF-Token'],'csrf');
  assert.equal(elements.get('service-token').textContent,'sk-new-<img>');
  const row=run('keyGroups.get("service:inactive").body.children[0]');
  assert.equal(row.children[1].textContent,'svc');assert.equal(row.children[5].textContent,'7');
  assert.match(row.children[4].textContent,/No models/);assert.doesNotMatch(row.textContent,/sk-new/);
  assert.equal(elements.get('count').textContent,'1');
  listeners.get('pagehide')();assert.equal(elements.get('service-token').textContent,'');
});

test('history is lazy, scoped to stable ID, safely text-rendered and retryable', async () => {
  const {context,run}=page(adminHTML);
  const calls=[];
  context.fetch=async(path)=>{calls.push(path);return {ok:false,status:503,json:async()=>({error:'store_unavailable'})};};
  const details=run('historyControls({key_id:"stable/key",name:"do not send"})');
  assert.equal(calls.length,0);
  const button=descendants(details).find(n=>n.tag==='button');
  await button.onclick();assert.equal(button.disabled,false);assert.match(details.textContent,/unavailable/);
  context.fetch=async(path)=>{calls.push(path);return {ok:true,json:async()=>({events:[{id:4,created_at:'2026-01-01T00:00:00Z',action:'<img src=x>',actor:{kind:'admin',id:'opaque'}}]})};};
  await button.onclick();
  const query=new URL(calls[1],'https://portal.example.com').searchParams;
  assert.equal(query.get('key_id'),'stable/key');assert.equal(query.get('before'),'0');assert.ok(!calls[1].includes('do not send'));
  assert.match(details.textContent,/<img src=x>/);assert.ok(!descendants(details).some(n=>n.tag==='img'));
  assert.equal(button.hidden,true);
});

test('service grant editor includes revision to reject pre-rotation edits', async () => {
 const {context,run}=page(adminHTML);const calls=[];
 context.fetch=async(path,init)=>{calls.push([path,init]);return {ok:true,json:async()=>({models:['m']})};};
 const td=run('modelControls({key_id:"svc",kind:"service",name:"Worker",revision:7,state:"active",models:[]},["m"])');
 const details=td.children[0];details.open=true;details.ontoggle();
 descendants(details).find(n=>n.tag==='input').checked=true;
 await descendants(details).find(n=>n.tag==='button').onclick();
 assert.deepEqual(JSON.parse(calls[0][1].body),{key_id:'svc',models:['m'],revision:7});
});

for (const navigation of ['refresh','navigate']) test('late rotate after '+navigation+' cannot restore stale rows or secrets', async () => {
 const {context,elements,run,location}=page(adminHTML,'?tab=users');
 context.key={key_id:'svc',name:'Worker',kind:'service',revision:1,state:'active',models:[],stats:{}};
 run(`signedIn=true;usersLoaded=true;renderOverview({since:'2026-01-01',until:'2026-02-01',stats:{},keys:[key]},[])`);
 let resolve;context.fetch=path=>new Promise(r=>{if(path.includes("service-keys/rotate"))resolve=r;});
 const button=descendants(elements.get('keys')).find(n=>n.tag==='button'&&n.textContent==='Rotate secret');
 const pending=button.onclick();
 if(navigation==='refresh')run('renderOverview({since:"2026-01-01",until:"2026-02-01",stats:{},keys:[{...key,revision:2,state:"revoked"}]},[])');
 else {location.search='?tab=overview';run('showRoute()');}
 resolve({ok:true,json:async()=>({key_id:'svc',name:'Worker',kind:'service',revision:2,state:'active',models:[],key:'secret'})});
 await pending;
 if(navigation==='refresh'){
  assert.equal(elements.get('keys').querySelectorAll('tr[data-search]').length,1);
  assert.match(elements.get('keys').textContent,/revoked/);
 }else assert.equal(elements.get('service-token').textContent,'');
});
