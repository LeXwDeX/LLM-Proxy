const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');

const source = fs.readFileSync(path.join(__dirname, 'js', 'clients.js'), 'utf8');
const start = source.indexOf('  function renderClients() {');
const end = source.indexOf('\n  }\n', start) + 4;
assert(start >= 0 && end > start, 'renderClients source should be present');
const styles = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
for (const rule of ['.client-target-list', '.client-target-direct-tag', '.client-target-pool-tag']) {
  assert(styles.includes(rule), 'stylesheet defines ' + rule);
}
// Long target/pool lists must scroll horizontally instead of being clipped by
// the panel's overflow:hidden.
const clientsPanel = styles.slice(styles.lastIndexOf('<div class="panel-body', styles.indexOf('id="clientsTableBody"')), styles.indexOf('id="clientsTableBody"'));
assert.match(clientsPanel, /^<div class="panel-body[^"]*\btable-wrap\b/, 'clients table scrolls horizontally');

function render(clients) {
  const tbody = {innerHTML: ''};
  const document = {getElementById: () => tbody};
  const state = {clients, clientUsage: {}};
  const esc = value => String(value == null ? '' : value).replace(/[&<>"']/g, ch => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[ch]));
  const fn = new Function('document', 'state', 'esc', 'maskKey', 'totalTok', 'fmtNum', 'fmtMoney', 'poolDisplayName', 'targetPoolsAvailable',
    source.slice(start, end) + '\nreturn renderClients;');
  fn(document, state, esc, () => '***', () => 0, () => '0', () => '$0', name => name, () => true)();
  return tbody.innerHTML;
}

const combined = render([{name: 'client', allowed_targets: ['qwen-max', 'glm-flash'], allowed_pools: ['pool A']}]);
assert.match(combined, /class=['"]client-target-list['"]/);
assert.match(combined, /class=['"][^'"]*client-target-direct/);
assert.match(combined, /class=['"][^'"]*client-target-pool/);
assert.ok(combined.indexOf('client-target-direct') < combined.indexOf('client-target-pool'), 'direct targets render above pools');
assert.match(combined, /qwen-max/);
assert.match(combined, /glm-flash/);
assert.match(combined, /class='client-target-pool-tag'><svg[\s\S]*?<\/svg> pool A/);
assert.doesNotMatch(combined, /🧩/, 'pool tag uses an inline SVG icon, not an emoji');

const directOnly = render([{name: 'client', allowed_targets: ['qwen-max']}]);
assert.match(directOnly, /class=['"][^'"]*client-target-direct/);
assert.doesNotMatch(directOnly, /client-target-pool/);
assert.doesNotMatch(directOnly, /client-target-list[^>]*>\s*<div[^>]*><\/div>/);

const poolOnly = render([{name: 'client', allowed_pools: ['pool A']}]);
assert.match(poolOnly, /class=['"][^'"]*client-target-pool/);
assert.doesNotMatch(poolOnly, /client-target-direct/);

const unrestricted = render([{name: 'client'}]);
assert.match(unrestricted, /全部目标/);

const escaped = render([{name: '<client>', allowed_targets: ['<qwen>'], allowed_pools: ['<pool>']}]);
assert.match(escaped, /&lt;qwen&gt;/);
assert.match(escaped, /class='client-target-pool-tag'><svg[\s\S]*?<\/svg> &lt;pool&gt;/);
assert.match(escaped, /&lt;client&gt;/);
console.log('client-access rendering checks passed');
