/* OmniDrive UI.
   Plain framework-free JavaScript: no build step, no CDN, embedded in the Go
   binary and served from localhost. */

'use strict';

// Only needed when the server is bound to a non-loopback address; the CLI
// prints the URL with the token already attached.
const TOKEN = new URLSearchParams(location.search).get('t') || '';

// Present only inside the Android app, which exposes the bits of the platform
// a web page cannot reach on its own.
const ANDROID = window.OmniDriveAndroid || null;

const state = {
  view: 'files',
  trail: [],            // [{accountId, folderId, name}] — current path
  accounts: [],
  providers: [],
  settings: null,
  jobs: new Map(),
  files: [],            // what is currently listed
  selection: new Set(),
  selectMode: false,
  sort: 'name',         // name | size | date | type
  sortDesc: false,
  filter: 'all',        // all | folders | images | video | audio | docs | archives
  query: '',
  searchAll: false,
};

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

/* ---------------- api ---------------- */

async function api(path, opts = {}) {
  const url = new URL(path, location.origin);
  if (TOKEN) url.searchParams.set('t', TOKEN);

  const res = await fetch(url, {
    ...opts,
    headers: {
      ...(opts.body && !(opts.body instanceof FormData) ? { 'Content-Type': 'application/json' } : {}),
      ...(opts.headers || {}),
    },
  });

  const type = res.headers.get('content-type') || '';
  if (!res.ok) {
    let msg = res.statusText;
    if (type.includes('json')) {
      try { msg = (await res.json()).error || msg; } catch { /* keep statusText */ }
    } else {
      try { msg = (await res.text()) || msg; } catch { /* keep statusText */ }
    }
    throw new Error(msg);
  }
  if (type.includes('json')) return res.json();
  if (type.includes('octet-stream')) return res.blob();
  return res.text();
}

const post = (path, body) => api(path, { method: 'POST', body: JSON.stringify(body) });

/* ---------------- formatting ---------------- */

function bytes(n) {
  if (!n || n < 0) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n < 10 && i > 0 ? n.toFixed(1) : Math.round(n)} ${units[i]}`;
}

function when(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime()) || d.getFullYear() < 1980) return '';
  const days = (Date.now() - d.getTime()) / 86400000;
  if (days < 1) return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  if (days < 335) return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
  return d.toLocaleDateString([], { year: 'numeric', month: 'short' });
}

const EXT_GROUPS = {
  images: ['jpg', 'jpeg', 'png', 'gif', 'webp', 'heic', 'avif', 'bmp', 'svg', 'tiff'],
  video: ['mp4', 'mkv', 'mov', 'webm', 'avi', 'm4v', '3gp', 'flv', 'wmv'],
  audio: ['mp3', 'flac', 'wav', 'm4a', 'ogg', 'opus', 'aac', 'wma'],
  docs: ['pdf', 'doc', 'docx', 'odt', 'txt', 'md', 'rtf', 'xls', 'xlsx', 'csv', 'ppt', 'pptx'],
  archives: ['zip', 'rar', '7z', 'tar', 'gz', 'xz', 'bz2', 'apk', 'iso'],
};

function extOf(name) { return (String(name).split('.').pop() || '').toLowerCase(); }

function groupOf(file) {
  if (file.isDir) return 'folders';
  const e = extOf(file.name);
  for (const [group, list] of Object.entries(EXT_GROUPS)) {
    if (list.includes(e)) return group;
  }
  return 'other';
}

function iconFor(file) {
  if (file.isDir) return '📁';
  switch (groupOf(file)) {
    case 'images': return '🖼️';
    case 'video': return '🎬';
    case 'audio': return '🎵';
    case 'archives': return '📦';
    case 'docs': return extOf(file.name) === 'pdf' ? '📕' : '📄';
    default: return '📄';
  }
}

const KIND_LABEL = {
  gdrive: 'Google Drive', onedrive: 'OneDrive', dropbox: 'Dropbox',
  pcloud: 'pCloud', terabox: 'TeraBox', webdav: 'WebDAV', s3: 'S3',
  local: 'This device',
};

/* Drives are labelled by the signed-in email, which is noisy in a file list
   and impossible to tell apart at a glance when several accounts share a
   provider. Show "Drive 1", "Drive 2" … unless the user has renamed it. */
function driveName(acc, index) {
  if (!acc) return '';
  if (acc.kind === 'pool' || acc.id === 'pool') return acc.label || 'All drives';
  const looksAuto = /@/.test(acc.label) || acc.label === KIND_LABEL[acc.kind] || /^\//.test(acc.label);
  if (!looksAuto) return acc.label;
  if (acc.kind === 'local') return acc.label.split(/[\\/]/).filter(Boolean).pop() || acc.label;
  return `Drive ${index + 1}`;
}

/* "1.2 GB / 15 GB" reads at a glance; "used" and "total" on opposite sides of
   a card do not. Falls back to just the used figure where the provider has no
   concept of a total, such as an S3 bucket. */
function quotaText(acc) {
  if (!acc) return '';
  if (acc.quotaTotal > 0) return `${bytes(acc.quotaUsed)} / ${bytes(acc.quotaTotal)}`;
  if (acc.quotaUsed > 0) return `${bytes(acc.quotaUsed)} used`;
  return '';
}

function quotaPercent(acc) {
  if (!acc || acc.quotaTotal <= 0) return 0;
  return Math.min(100, (acc.quotaUsed / acc.quotaTotal) * 100);
}

function driveSubtitle(acc) {
  if (acc.kind === 'pool' || acc.id === 'pool') {
    const q = quotaText(acc);
    const n = acc.drives || 0;
    return [`${n} drive${n === 1 ? '' : 's'} combined`, q].filter(Boolean).join(' · ');
  }
  // For device storage show the folder rather than the provider name: it is
  // the only way to tell two local drives apart at a glance.
  const parts = [acc.kind === 'local' ? (acc.displayName || acc.label) : (KIND_LABEL[acc.kind] || acc.kind)];
  const q = quotaText(acc);
  if (q) parts.push(q);
  return parts.join(' · ');
}

function accountIndex(id) {
  return state.accounts.findIndex((a) => a.id === id);
}

function escapeHtml(s) {
  return String(s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

function toast(msg, kind = '') {
  const el = document.createElement('div');
  el.className = `toast ${kind}`;
  el.textContent = msg;
  $('#toasts').append(el);
  setTimeout(() => el.remove(), kind === 'error' ? 7000 : 3500);
}

const fail = (err) => toast(err.message || String(err), 'error');

/* ---------------- bottom sheet ---------------- */

function sheet(title, items) {
  const panel = $('#sheet-panel');
  panel.innerHTML = '<div class="sheet-grab"></div>';

  const h = document.createElement('h3');
  h.textContent = title;
  panel.append(h);

  for (const item of items) {
    if (!item) continue;
    const b = document.createElement('button');
    b.className = `sheet-item ${item.danger ? 'danger' : ''}`;
    b.innerHTML = `<span style="width:22px;text-align:center;flex:none">${item.icon || ''}</span>`;

    // A label may carry a second explanatory line after a newline.
    const [title2, detail] = String(item.label).split('\n');
    const text = document.createElement('span');
    text.append(document.createTextNode(title2));
    if (detail) {
      const sub = document.createElement('span');
      sub.className = 'muted small';
      sub.style.display = 'block';
      sub.style.marginTop = '3px';
      sub.textContent = detail;
      text.append(sub);
    }
    b.append(text);
    b.onclick = () => { closeSheet(); item.run(); };
    panel.append(b);
  }
  $('#sheet').hidden = false;
}

const closeSheet = () => { $('#sheet').hidden = true; };

$('#sheet').addEventListener('click', (e) => {
  if (e.target.dataset.close !== undefined) closeSheet();
});

/* A prompt built on the sheet, so text entry matches the rest of the UI rather
   than using the browser's blocking prompt(), which Android styles badly. */
function ask(title, fields, onSubmit) {
  const panel = $('#sheet-panel');
  panel.innerHTML = '<div class="sheet-grab"></div>';

  const h = document.createElement('h3');
  h.textContent = title;
  panel.append(h);

  const inputs = fields.map((f) => {
    const input = document.createElement('input');
    input.className = 'input';
    input.placeholder = f.placeholder || f.label;
    input.type = f.type || 'text';
    input.value = f.value || '';
    if (f.autocaps) input.setAttribute('autocapitalize', 'characters');
    input.autocomplete = 'off';
    panel.append(input);
    return input;
  });

  const go = document.createElement('button');
  go.className = 'btn primary block';
  go.textContent = 'Continue';
  go.onclick = () => {
    const values = inputs.map((i) => i.value.trim());
    closeSheet();
    onSubmit(values);
  };
  panel.append(go);

  $('#sheet').hidden = false;
  setTimeout(() => inputs[0]?.focus(), 60);
}

function confirmSheet(title, actionLabel, run, note) {
  sheet(title, [
    { icon: '✓', label: note ? `${actionLabel}\n${note}` : actionLabel, danger: true, run },
    { icon: '↩', label: 'Cancel', run: () => {} },
  ]);
}

/* ---------------- navigation ---------------- */

function switchView(view) {
  state.view = view;
  for (const el of $$('.view')) el.hidden = true;
  $(`#view-${view}`).hidden = false;

  for (const t of $$('.tab')) t.classList.toggle('active', t.dataset.view === view);

  const titles = { files: 'Files', drives: 'Drive Settings', sync: 'Share' };
  $('#view-title').textContent = titles[view] || 'OmniDrive';

  const onFiles = view === 'files';
  $('#crumbs').hidden = !onFiles;
  $('#fab-wrap').hidden = !onFiles;
  // The file-list controls mean nothing on the other screens.
  for (const id of ['#btn-search', '#btn-more']) {
    $(id).hidden = !onFiles;
  }
  if (!onFiles) exitSelectMode();

  if (view === 'files') loadFiles();
  if (view === 'drives') { loadDrives(); loadSettings(); }
  if (view === 'sync') loadSync();
  syncBackGuard();
}

for (const tab of $$('.tab')) tab.onclick = () => switchView(tab.dataset.view);


$('#btn-up').onclick = () => goUp();

function goUp() {
  if (state.query) { closeSearch(); return; }
  if (state.trail.length === 0) return;
  state.trail.pop();
  loadFiles();
}

/* Android's back gesture should walk up the app, not leave it.
   Rather than pushing a history entry at each navigation — which is easy to
   forget somewhere, and did leave "browse from the Drives tab" with no entry
   to pop — keep exactly one spare entry whenever there is anywhere to go back
   to. Back then always has something to consume, and when the app genuinely
   has nowhere left to go the entry is absent and Android closes it. */

history.replaceState({ root: true }, '');

function canGoBackInApp() {
  return !$('#viewer').hidden || !$('#picker').hidden || !$('#sheet').hidden ||
    !$('#guide').hidden ||
    state.selectMode || !!state.query || state.trail.length > 0 || state.view !== 'files';
}

function syncBackGuard() {
  if (canGoBackInApp()) {
    if (!history.state || !history.state.guard) history.pushState({ guard: true }, '');
  }
}

// Returns true when the press was handled inside the app. Ordered
// innermost-first, so back always dismisses the thing on top.
function handleAppBack() {
  if (!$('#viewer').hidden) { closeViewer(); return true; }
  if (!$('#sheet').hidden) { closeSheet(); return true; }
  if (!$('#guide').hidden) {
    if (guide.provider) { guide.provider = null; renderGuide(); }
    else closeGuide();
    return true;
  }
  if (!$('#picker').hidden) {
    if (picker.trail.length) { picker.trail.pop(); loadPicker(); }
    else closePicker();
    return true;
  }
  if (state.selectMode) { exitSelectMode(); return true; }
  if (state.query) { closeSearch(); return true; }
  if (state.view === 'files' && state.trail.length > 0) { goUp(); return true; }
  if (state.view !== 'files') { switchView('files'); return true; }
  return false;
}

window.addEventListener('popstate', () => {
  if (handleAppBack()) syncBackGuard();
});

/* The Android wrapper calls this on every back press and only closes the app
   when it returns false. Going through the page rather than the WebView's own
   history is what makes back reliable: pushState entries are not dependably
   visible to WebView.canGoBack(), so the app could be several screens deep and
   still be closed outright. */
window.omniBack = function omniBack() {
  try {
    return handleAppBack();
  } catch (err) {
    // Never trap the user because of a UI error.
    return false;
  }
};

/* ---------------- files ---------------- */

function currentLocation() {
  const last = state.trail[state.trail.length - 1];
  return last ? { account: last.accountId, folder: last.folderId } : { account: '', folder: '' };
}

function renderCrumbs() {
  const nav = $('#crumbs');
  nav.innerHTML = '';
  const mk = (label, index) => {
    const b = document.createElement('button');
    b.className = 'crumb';
    b.textContent = label;
    b.onclick = () => { state.trail = state.trail.slice(0, index); loadFiles(); };
    nav.append(b);
  };
  mk('All drives', 0);
  state.trail.forEach((c, i) => mk(c.name, i + 1));
  $('#btn-up').hidden = state.trail.length === 0 && !state.query;
  syncBackGuard();
}

async function loadFiles() {
  renderCrumbs();
  exitSelectMode();
  const list = $('#files-list');
  const empty = $('#files-empty');
  list.innerHTML = '';
  empty.hidden = true;

  // Keep the account list fresh so drive numbering and names stay correct.
  if (!state.accounts.length) {
    try { state.accounts = await api('/api/accounts'); } catch { /* non-fatal */ }
  }

  if (state.query) return runSearch();

  const loc = currentLocation();
  try {
    const q = new URLSearchParams();
    if (loc.account) q.set('account', loc.account);
    if (loc.folder) q.set('folder', loc.folder);
    const data = await api(`/api/files?${q}`);
    state.files = data.files || [];
    // Combined-drive figures ride along with the root listing, and with any
    // listing inside the pool, so the header can show one storage total.
    if (data.pooled) state.pooled = data.pooled;
    else if (data.pool && data.account) state.pooled = data.account;

    if (data.root) return renderRoot(state.files);
    renderFiles(applyView(state.files));
  } catch (err) {
    fail(err);
    empty.textContent = 'Could not load this folder.';
    empty.hidden = false;
  }
}

/* The root shows each connected drive. Merging every provider into one
   namespace produces duplicate names and ambiguous moves; showing the drives
   explicitly is honest and navigable. */
function renderRoot(files) {
  const list = $('#files-list');
  const empty = $('#files-empty');
  // Clear here rather than trusting the caller: renderCurrent() reaches this
  // directly when the view mode or sort changes, and without this every such
  // change appended another copy of every drive.
  list.innerHTML = '';

  if (!files.length) {
    empty.innerHTML = 'No drives yet.<br><br>Open the <b>Drives</b> tab to connect a cloud account, or add your phone storage.';
    empty.hidden = false;
    return;
  }
  const pooled = files.filter((f) => f.accountId === 'pool');
  const rest = files.filter((f) => f.accountId !== 'pool');
  const cloud = rest.filter((f) => accountKind(f.accountId) !== 'local');
  const device = rest.filter((f) => accountKind(f.accountId) === 'local');

  // Device storage first — phone, then SD card — then the cloud. No section
  // headings: a file manager lists drives, it does not caption them, and in
  // the tile view a heading has to span the whole grid and leaves a gap.
  const frag = document.createDocumentFragment();
  device.forEach((f) => frag.append(driveRow(f)));
  pooled.forEach((f) => frag.append(driveRow(f)));
  cloud.forEach((f) => frag.append(driveRow(f)));
  list.append(frag);
}

function accountKind(id) {
  return state.accounts.find((a) => a.id === id)?.kind || '';
}


function driveRow(file) {
  // The pool is not a stored account; the root listing carries its figures.
  const isPool = file.accountId === 'pool';
  const acc = isPool ? state.pooled : state.accounts.find((a) => a.id === file.accountId);
  const idx = accountIndex(file.accountId);
  const name = driveName(acc, idx);
  const sub = acc ? driveSubtitle(acc) : '';
  const pct = quotaPercent(acc);

  const row = document.createElement('div');
  row.className = 'row-item';
  row.innerHTML = `
    <div class="row-icon">${isPool ? '🗄️' : (acc?.kind === 'local' ? '📱' : '☁️')}</div>
    <div class="row-body">
      <div class="row-name">${escapeHtml(name)}</div>
      <div class="row-sub">${escapeHtml(sub)}</div>
      ${pct > 0 ? `<div class="meter ${pct > 90 ? 'full' : ''}" style="margin-top:6px"><span style="width:${pct}%"></span></div>` : ''}
    </div>`;

  if (!isPool) {
    const more = document.createElement('button');
    more.className = 'row-more';
    more.textContent = '⋯';
    more.onclick = (e) => { e.stopPropagation(); driveMenu(acc, name); };
    row.append(more);
  }

  row.onclick = () => openFolder(file.accountId, '', name);
  return row;
}

function openFolder(accountId, folderId, name) {
  state.trail.push({ accountId, folderId, name });
  loadFiles();
}

/* applyView filters and sorts the current listing without refetching. */
function applyView(files) {
  let out = files.slice();

  if (state.filter === 'folders') out = out.filter((f) => f.isDir);
  else if (state.filter === 'starred') out = out.filter((f) => f.starred);
  else if (state.filter !== 'all') out = out.filter((f) => groupOf(f) === state.filter);

  const dir = state.sortDesc ? -1 : 1;
  out.sort((a, b) => {
    // Folders always lead, regardless of sort key: that is what every file
    // manager does and what makes a listing navigable.
    if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
    switch (state.sort) {
      case 'size': return (a.size - b.size) * dir;
      case 'date': return (new Date(a.modified) - new Date(b.modified)) * dir;
      case 'type': return (extOf(a.name).localeCompare(extOf(b.name)) || a.name.localeCompare(b.name)) * dir;
      default: return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' }) * dir;
    }
  });
  return out;
}

function renderFiles(files, opts = {}) {
  const list = $('#files-list');
  const empty = $('#files-empty');
  list.innerHTML = '';

  if (!files.length) {
    empty.textContent = state.filter === 'all'
      ? 'This folder is empty.'
      : 'Nothing here matches that filter.';
    empty.hidden = false;
    return;
  }
  empty.hidden = true;
  // Build off-document and attach once: appending row by row forces a reflow
  // per file, which is what made large folders crawl.
  const frag = document.createDocumentFragment();
  files.forEach((f) => frag.append(fileRow(f, opts)));
  list.append(frag);
}

function fileRow(file, opts = {}) {
  const row = document.createElement('div');
  row.className = 'row-item';
  const key = `${file.accountId}/${file.id}`;
  if (state.selection.has(key)) row.classList.add('selected');

  const bits = [];
  if (opts.showDrive) {
    const acc = state.accounts.find((a) => a.id === file.accountId);
    bits.push(driveName(acc, accountIndex(file.accountId)));
  }
  bits.push(file.isDir ? 'Folder' : bytes(file.size));
  const t = when(file.modified);
  if (t) bits.push(t);

  // The meta column is rendered always but shown only in Details view, so
  // switching modes needs no re-fetch.
  const meta = file.isDir
    ? `<div>Folder</div><div>${escapeHtml(when(file.modified))}</div>`
    : `<div>${escapeHtml(bytes(file.size))}</div><div>${escapeHtml(when(file.modified))}</div>`;

  row.innerHTML = `
    <div class="checkbox">✓</div>
    <div class="row-icon">${iconFor(file)}</div>
    <div class="row-body">
      <div class="row-name">${file.starred ? '<span class="star-on">★ </span>' : ''}${escapeHtml(file.name)}</div>
      <div class="row-sub">${escapeHtml(bits.join(' · '))}</div>
    </div>
    <div class="row-meta">${meta}</div>`;

  const more = document.createElement('button');
  more.className = 'row-more';
  more.textContent = '⋯';
  more.onclick = (e) => { e.stopPropagation(); fileMenu(file); };
  row.append(more);

  row.onclick = () => {
    if (Date.now() < suppressClickUntil) return;
    if (state.selectMode) return toggleSelect(file, row);
    if (file.isDir) openFolder(file.accountId, file.id, file.name);
    else if (canView(file)) openViewer(file);
    else openUnviewable(file);
  };

  // Long press starts selection, as in every mobile file manager.
  let timer = null;
  const cancel = () => { clearTimeout(timer); timer = null; };
  row.addEventListener('touchstart', () => {
    timer = setTimeout(() => selectFromLongPress(file, row), 450);
  }, { passive: true });
  row.addEventListener('touchend', cancel, { passive: true });
  row.addEventListener('touchmove', cancel, { passive: true });
  row.addEventListener('contextmenu', (e) => {
    // Android fires this after a long press as well as the timer above;
    // selecting is idempotent, but the default menu must not appear.
    e.preventDefault();
    selectFromLongPress(file, row);
  });

  return row;
}

/* ---------------- selection ---------------- */

// Returns true if selection mode is active afterwards.
//
// Entering the mode only toggles a class on the list. It used to re-render
// every row, which moved the list under the user's finger mid-press — so the
// click that followed a long press landed on a *different* row and selected a
// second file. Nothing moves now.
function enterSelectMode() {
  if (state.selectMode) return true;
  // Nothing at the top level is selectable — those rows are drives, not files.
  if (state.view === 'files' && state.trail.length === 0 && !state.query) {
    toast('Open a drive first, then select files');
    return false;
  }
  state.selectMode = true;
  $('#files-list').classList.add('selecting');
  // `.on`, not `.spinning`: a spinner says "working", which is misleading for
  // a mode toggle that is waiting for the user to tap something.
  syncBackGuard();
  return true;
}

/* A long press selects the file it was pressed on — and only that one.
   The click that Android synthesises when the finger lifts is swallowed,
   or it would immediately toggle whatever row it landed on. */
let suppressClickUntil = 0;

function selectFromLongPress(file, row) {
  if (!enterSelectMode()) return; // refused, e.g. at the drive list

  const key = `${file.accountId}/${file.id}`;
  state.selection.add(key);
  row.classList.add('selected');
  updateSelBar();

  suppressClickUntil = Date.now() + 700;
}

function exitSelectMode() {
  if (!state.selectMode && state.selection.size === 0) return;
  state.selectMode = false;
  state.selection.clear();
  $('#files-list').classList.remove('selecting');
  for (const el of $$('.row-item.selected')) el.classList.remove('selected');
  $('#selbar').hidden = true;
}

function renderCurrent() {
  if (state.query) renderFiles(state.searchResults || [], { showDrive: true });
  else if (state.trail.length === 0) renderRoot(state.files);
  else renderFiles(applyView(state.files));
}

function toggleSelect(file, row) {
  const key = `${file.accountId}/${file.id}`;
  if (state.selection.has(key)) state.selection.delete(key);
  else state.selection.add(key);
  row.classList.toggle('selected', state.selection.has(key));
  updateSelBar();
}

function updateSelBar() {
  const n = state.selection.size;
  $('#sel-count').textContent = n;
  $('#selbar').hidden = n === 0;
  if (n === 0 && state.selectMode) return;
}

// Selection is reached from the overflow menu, or by long-pressing a file.

function selectedFiles() {
  const src = state.query ? (state.searchResults || []) : state.files;
  return src.filter((f) => state.selection.has(`${f.accountId}/${f.id}`));
}

for (const btn of $$('.selbtn')) {
  btn.onclick = () => {
    const files = selectedFiles();
    if (!files.length) return;
    switch (btn.dataset.act) {
      case 'copy': chooseTarget(files, false); break;
      case 'move': chooseTarget(files, true); break;
      case 'share': shareSelection(files); break;
      case 'download': files.forEach((f) => !f.isDir && downloadFile(f)); exitSelectMode(); break;
      case 'delete': deleteFiles(files); break;
    }
  };
}

/* Deleting means different things on the two halves of this app, and the
   confirmation has to say which one the user is about to get.

   Cloud: gone for good, space back immediately — leaving files in a vendor's
   bin would keep eating the quota you pay for, and emptying it would mean
   opening their website.
   Phone and SD card: into OmniDrive's own recycle bin, restorable from Drive
   Settings, because nothing else on the device offers a second chance. */
function deleteKindFor(files) {
  let local = false;
  let cloud = false;
  for (const f of files) {
    const acc = state.accounts.find((a) => a.id === f.accountId);
    if (acc?.kind === 'local') local = true;
    else cloud = true; // includes pooled folders, which span cloud drives
  }
  if (local && !cloud) return 'bin';
  if (cloud && !local) return 'gone';
  return 'mixed';
}

function deleteFiles(files) {
  const names = files.length === 1 ? `"${files[0].name}"` : `${files.length} items`;
  const note = {
    bin: 'Goes to the recycle bin. Restore it from Drive Settings.',
    gone: 'Permanently deleted and the space freed. This cannot be undone.',
    mixed: 'Cloud files are deleted for good; phone and SD card files go to the recycle bin.',
  }[deleteKindFor(files)];
  confirmSheet(`Delete ${names}?`, 'Delete', () => runDelete(files), note);
}

async function runDelete(files) {
  // Group by drive: the API deletes within one account at a time.
  const byAccount = new Map();
  for (const f of files) {
    if (!byAccount.has(f.accountId)) byAccount.set(f.accountId, []);
    byAccount.get(f.accountId).push(f.id);
  }

  let failed = 0;
  for (const [account, ids] of byAccount) {
    try {
      const res = await post('/api/files/delete', { account, ids });
      failed += (res.results || []).filter((r) => !r.ok).length;
    } catch { failed += ids.length; }
  }

  if (failed) toast(`${failed} item(s) could not be deleted`, 'error');
  else toast('Deleted', 'ok');

  exitSelectMode();
  loadFiles();
  // The freed space only shows once the drive is re-measured.
  api('/api/accounts/refresh', { method: 'POST' }).catch(() => {});
}

/* Copy or move to another drive. The destination is chosen by browsing the
   target's folders — dumping everything in the root is not what anyone means
   by "move to my Drive". */
/* Group a selection by the drive each item lives on.
   In the combined view one screenful routinely spans several accounts, so
   refusing a mixed selection — as this used to — made multi-select useless
   there. The transfer API works one source at a time, so issue one call per
   group instead. */
function groupBySource(files) {
  const groups = new Map();
  for (const f of files) {
    if (!groups.has(f.accountId)) groups.set(f.accountId, []);
    groups.get(f.accountId).push(f);
  }
  return groups;
}

async function chooseTarget(files, move) {
  const groups = groupBySource(files);
  // Only exclude the source drive when everything came from one place;
  // otherwise every destination is legitimate.
  const exclude = groups.size === 1 ? files[0].accountId : '';

  let targets;
  try {
    targets = await api(`/api/transfer/targets?exclude=${encodeURIComponent(exclude)}`);
  } catch (err) { return fail(err); }

  if (!targets || !targets.length) {
    return toast('Connect another drive first', 'error');
  }
  sheet(`${move ? 'Move' : 'Copy'} ${files.length} item(s) to…`, targets.map((t) => ({
    icon: t.isLocal ? '📱' : '☁️',
    label: `${driveName(t, accountIndex(t.id))}\n${driveSubtitle(t)}`,
    run: () => openPicker(t, files, move),
  })));
}

/* ---------------- destination picker ---------------- */

const picker = { account: null, trail: [], files: [], move: false, target: null };

function openPicker(account, files, move) {
  picker.account = account;
  picker.files = files;
  picker.move = move;
  picker.trail = [];
  $('#picker').hidden = false;
  $('#picker-confirm').textContent = move ? 'Move here' : 'Copy here';
  syncBackGuard();
  loadPicker();
}

function closePicker() { $('#picker').hidden = true; }

$('#picker-cancel').onclick = closePicker;
$('#picker-up').onclick = () => {
  if (!picker.trail.length) return closePicker();
  picker.trail.pop();
  loadPicker();
};

async function loadPicker() {
  const acc = picker.account;
  const idx = accountIndex(acc.id);
  const here = picker.trail[picker.trail.length - 1];
  $('#picker-title').textContent = here ? here.name : driveName(acc, idx);

  const crumbs = $('#picker-crumbs');
  crumbs.innerHTML = '';
  const mk = (label, i) => {
    const b = document.createElement('button');
    b.className = 'crumb';
    b.textContent = label;
    b.onclick = () => { picker.trail = picker.trail.slice(0, i); loadPicker(); };
    crumbs.append(b);
  };
  mk(driveName(acc, idx), 0);
  picker.trail.forEach((c, i) => mk(c.name, i + 1));

  const list = $('#picker-list');
  list.innerHTML = '<div class="empty">Loading…</div>';
  try {
    const q = new URLSearchParams({ account: acc.id });
    if (here) q.set('folder', here.id);
    const data = await api(`/api/files?${q}`);

    // Only folders are destinations.
    const folders = (data.files || []).filter((f) => f.isDir);
    list.innerHTML = '';
    if (!folders.length) {
      list.innerHTML = '<div class="empty">No sub-folders here.<br>Use <b>New folder</b>, or paste into this one.</div>';
      return;
    }
    for (const f of folders) {
      const row = document.createElement('div');
      row.className = 'row-item';
      row.innerHTML = `
        <div class="row-icon">📁</div>
        <div class="row-body"><div class="row-name">${escapeHtml(f.name)}</div></div>
        <div class="row-more">›</div>`;
      row.onclick = () => { picker.trail.push({ id: f.id, name: f.name }); loadPicker(); };
      list.append(row);
    }
  } catch (err) {
    list.innerHTML = '<div class="empty">Could not open this folder.</div>';
    fail(err);
  }
}

$('#picker-newfolder').onclick = () => {
  const here = picker.trail[picker.trail.length - 1];
  ask('New folder', [{ label: 'Folder name', placeholder: 'Folder name' }], async ([name]) => {
    if (!name) return;
    try {
      const created = await post('/api/files/mkdir', {
        account: picker.account.id, parent: here ? here.id : '', name,
      });
      toast('Folder created', 'ok');
      // Step into it, which is almost always what you wanted next.
      picker.trail.push({ id: created.id, name: created.name || name });
      loadPicker();
    } catch (err) { fail(err); }
  });
};

$('#picker-confirm').onclick = async () => {
  const here = picker.trail[picker.trail.length - 1];
  const groups = groupBySource(picker.files);

  let started = 0;
  let failures = 0;
  for (const [fromAccount, files] of groups) {
    try {
      const res = await post('/api/transfer', {
        fromAccount,
        toAccount: picker.account.id,
        toFolder: here ? here.id : '',
        ids: files.map((f) => f.id),
        move: picker.move,
      });
      started += res.items;
    } catch (err) {
      failures++;
      fail(err);
    }
  }
  if (started > 0) {
    toast(`${picker.move ? 'Moving' : 'Copying'} ${started} item(s) to ${picker.account.label}`, 'ok');
  } else if (!failures) {
    toast('Those items are already on that drive', 'error');
  }
  closePicker();
  exitSelectMode();
};

/* ---------------- search ---------------- */

$('#btn-search').onclick = () => {
  const bar = $('#searchbar');
  bar.hidden = !bar.hidden;
  if (!bar.hidden) $('#search-input').focus();
  else closeSearch();
};

$('#search-close').onclick = () => closeSearch();

$('#search-scope').onclick = (e) => {
  state.searchAll = !state.searchAll;
  e.currentTarget.textContent = state.searchAll ? 'All drives' : 'This drive';
  e.currentTarget.classList.toggle('on', state.searchAll);
  if (state.query) runSearch();
};

let searchTimer = null;
$('#search-input').oninput = (e) => {
  clearTimeout(searchTimer);
  const q = e.target.value.trim();
  // Crawling a Drive over mobile data is expensive; wait for a pause.
  searchTimer = setTimeout(() => {
    state.query = q;
    if (!q) { loadFiles(); return; }
    runSearch();
  }, 450);
};

function closeSearch() {
  $('#searchbar').hidden = true;
  $('#search-input').value = '';
  if (state.query) { state.query = ''; state.searchResults = null; loadFiles(); }
}

async function runSearch() {
  const list = $('#files-list');
  const empty = $('#files-empty');
  list.innerHTML = '';
  empty.textContent = 'Searching…';
  empty.hidden = false;

  const loc = currentLocation();
  const q = new URLSearchParams({ q: state.query });
  if (!state.searchAll && loc.account) {
    q.set('account', loc.account);
    if (loc.folder) q.set('folder', loc.folder);
  }
  try {
    const data = await api(`/api/search?${q}`);
    state.searchResults = data.files || [];
    if (!state.searchResults.length) {
      empty.textContent = `Nothing matching "${state.query}".`;
      empty.hidden = false;
      return;
    }
    renderFiles(state.searchResults, { showDrive: true });
    syncBackGuard();
    if (data.truncated) toast('Showing the first 300 matches', '');
  } catch (err) {
    fail(err);
    empty.textContent = 'Search failed.';
    empty.hidden = false;
  }
}

/* ---------------- sort ---------------- */

/* No type-filter chips. A row of Images / Video / Audio / Documents buttons
   took permanent space at the top of every folder to answer a question people
   ask rarely — search does the same job on demand. Only "starred only"
   survives, folded into this sheet, because starring has no other home now. */

function showSortSheet() {
  const opt = (key, label) => ({
    icon: state.sort === key ? (state.sortDesc ? '↓' : '↑') : ' ',
    label,
    run: () => {
      if (state.sort === key) state.sortDesc = !state.sortDesc;
      else { state.sort = key; state.sortDesc = false; }
      renderCurrent();
    },
  });
  const starred = state.filter === 'starred';
  sheet('Sort by', [
    opt('name', 'Name'),
    opt('size', 'Size'),
    opt('date', 'Date modified'),
    opt('type', 'File type'),
    {
      icon: starred ? '★' : '☆',
      label: starred ? 'Showing starred only' : 'Show starred only',
      run: () => { state.filter = starred ? 'all' : 'starred'; renderCurrent(); },
    },
  ]);
}

/* ---------------- per-file actions ---------------- */

/* Content types the app needs to name correctly. Android decides which apps
   can open a file almost entirely from this — with no type, an http URL only
   ever matches browsers. Anything unlisted becomes "*​/*", which shows the
   full chooser rather than nothing. */
const MIME_BY_EXT = {
  mp4: 'video/mp4', m4v: 'video/mp4', mov: 'video/quicktime', mkv: 'video/x-matroska',
  webm: 'video/webm', avi: 'video/x-msvideo', '3gp': 'video/3gpp', ts: 'video/mp2t',
  mp3: 'audio/mpeg', m4a: 'audio/mp4', aac: 'audio/aac', flac: 'audio/flac',
  wav: 'audio/wav', ogg: 'audio/ogg', opus: 'audio/opus',
  jpg: 'image/jpeg', jpeg: 'image/jpeg', png: 'image/png', gif: 'image/gif',
  webp: 'image/webp', avif: 'image/avif', bmp: 'image/bmp', heic: 'image/heic',
  pdf: 'application/pdf', epub: 'application/epub+zip',
  apk: 'application/vnd.android.package-archive',
  zip: 'application/zip', rar: 'application/vnd.rar', '7z': 'application/x-7z-compressed',
  txt: 'text/plain', md: 'text/plain', log: 'text/plain', csv: 'text/csv',
  json: 'application/json', xml: 'application/xml',
  doc: 'application/msword',
  docx: 'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  xls: 'application/vnd.ms-excel',
  xlsx: 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  ppt: 'application/vnd.ms-powerpoint',
  pptx: 'application/vnd.openxmlformats-officedocument.presentationml.presentation',
};

function mimeFor(name) {
  return MIME_BY_EXT[extOf(name)] || '*/*';
}

function downloadFile(file) {
  const q = new URLSearchParams({ account: file.accountId, id: file.id, name: file.name });
  if (TOKEN) q.set('t', TOKEN);
  const url = `${location.origin}/api/files/download?${q}`;

  // Hand the exact filename to the Android side. Left to the WebView, an APK
  // or EXE is saved as "download.bin", because guessFileName invents a name
  // from the content type when it cannot read one.
  if (ANDROID && ANDROID.download) {
    ANDROID.download(url, file.name, mimeFor(file.name));
    return;
  }
  window.location.href = url;
}

function streamURL(file) {
  const q = new URLSearchParams({ account: file.accountId, id: file.id, name: file.name });
  if (TOKEN) q.set('t', TOKEN);
  return `${location.origin}/api/files/stream?${q}`;
}

/* ---------------- viewer ---------------- */

/* Tapping a file should open it, the way it does in a normal file manager.
   The stream endpoint serves byte ranges, so a two-hour film starts playing
   at once and seeks without downloading the whole thing. */

const VIEWABLE = new Set(['images', 'video', 'audio']);
const TEXT_EXT = ['txt', 'md', 'log', 'json', 'csv', 'xml', 'yml', 'yaml', 'ini',
  'js', 'ts', 'css', 'html', 'go', 'py', 'java', 'sh', 'c', 'h', 'cpp', 'rs', 'sql'];

function canView(file) {
  if (file.isDir) return false;
  return VIEWABLE.has(groupOf(file)) || TEXT_EXT.includes(extOf(file.name));
}

let currentViewFile = null;

function openViewer(file) {
  currentViewFile = file;
  syncBackGuard();
  const body = $('#viewer-body');
  const url = streamURL(file);
  body.innerHTML = '';
  $('#viewer-name').textContent = file.name;
  $('#viewer').hidden = false;
  // The external-app handoff only exists inside the Android app.
  $('#viewer-ext').hidden = !ANDROID;

  const group = groupOf(file);
  const ext = extOf(file.name);

  if (group === 'images') {
    const img = document.createElement('img');
    img.src = url;
    img.alt = file.name;
    img.onerror = () => viewerFallback('This image could not be displayed.');
    body.append(img);

  } else if (group === 'video') {
    const v = document.createElement('video');
    v.src = url;
    v.controls = true;
    v.autoplay = true;
    v.playsInline = true;
    // metadata only: the point is to start now, not to buffer the film.
    v.preload = 'metadata';
    v.onerror = () => viewerFallback(
      `This device cannot play ${ext.toUpperCase()} in the browser.`, true);
    body.append(v);

  } else if (group === 'audio') {
    const a = document.createElement('audio');
    a.src = url;
    a.controls = true;
    a.autoplay = true;
    a.onerror = () => viewerFallback('This audio format is not supported here.', true);
    body.append(a);

  } else if (TEXT_EXT.includes(ext)) {
    const pre = document.createElement('pre');
    pre.textContent = 'Loading…';
    body.append(pre);
    // Cap it: opening a 500 MB log would lock the page up.
    fetch(url, { headers: { Range: 'bytes=0-1048575' } })
      .then((r) => r.text())
      .then((text) => {
        pre.textContent = text;
        if (file.size > 1048576) pre.textContent += '\n\n… truncated at 1 MB';
      })
      .catch(() => viewerFallback('Could not read this file.'));

  } else {
    viewerFallback('No preview available for this file type.', true);
  }
}

function viewerFallback(message, offerExternal = false) {
  const body = $('#viewer-body');
  body.innerHTML = '';
  const box = document.createElement('div');
  box.className = 'viewer-msg';
  box.textContent = message;

  if (offerExternal && ANDROID) {
    const b = document.createElement('button');
    b.className = 'btn primary block';
    b.textContent = 'Open in another app';
    b.onclick = () => openExternally(currentViewFile);
    box.append(b);
  }
  const dl = document.createElement('button');
  dl.className = 'btn block';
  dl.textContent = 'Download instead';
  dl.onclick = () => { closeViewer(); downloadFile(currentViewFile); };
  box.append(dl);
  body.append(box);
}

/* Hands the stream URL to a real player. VLC and MX Player open http:// URLs
   directly, so a codec the WebView cannot decode still plays — and still
   streams rather than downloading. */
function openExternally(file) {
  if (!ANDROID || !file) return;
  try {
    // Name and size travel with it: the receiving app asks for both before
    // opening, and the size is what lets a player seek.
    ANDROID.openExternal(streamURL(file), file.name, String(file.size || 0), mimeFor(file.name));
    toast('Opening…');
  } catch { toast('No app available to open this', 'error'); }
}

/* Share files with any app on the device — Bluetooth, a chat app, email.
   Folders cannot be shared as a stream, so they are skipped. */
/* Two different things are called "share", and picking the wrong one wastes a
   lot of data: sending the files pushes every byte off this phone, whereas a
   link costs nothing and lets the other person fetch them from the cloud. */
function shareSelection(files) {
  const linkable = files.filter(canShareLink);
  if (!linkable.length) return shareFiles(files);

  sheet(files.length === 1 ? files[0].name : `${files.length} items`, [
    {
      icon: '🔗',
      label: `Copy link${linkable.length > 1 ? 's' : ''}\nNothing is uploaded — they download from the cloud`,
      run: () => shareLinks(linkable),
    },
    ANDROID && files.some((f) => !f.isDir) && {
      icon: '📨',
      label: 'Send the files\nUploads from this phone to the app you pick',
      run: () => shareFiles(files),
    },
  ]);
}

/* One link is copied on its own; several are copied as a list, one per line,
   which is what pasting into a chat should give you. */
async function shareLinks(files) {
  if (files.length === 1) return shareLink(files[0]);

  const lines = [];
  let failed = 0;
  toast(`Creating ${files.length} links…`);
  for (const f of files) {
    try {
      const res = await post('/api/files/share', {
        account: f.accountId, id: f.id, name: f.name, isDir: !!f.isDir,
      });
      lines.push(`${f.name}\n${res.direct || res.url}`);
    } catch { failed++; }
  }
  if (!lines.length) return toast('No links could be created', 'error');
  await copyText(lines.join('\n\n'));
  if (failed) toast(`${failed} of ${files.length} could not be shared`, 'error');
  exitSelectMode();
}

function shareFiles(files) {
  const sharable = files.filter((f) => !f.isDir);
  if (!sharable.length) return toast('Folders cannot be shared', 'error');
  if (!ANDROID || !ANDROID.shareFiles) {
    return toast('Sharing is only available in the app', 'error');
  }
  const records = sharable
    .map((f) => [streamURL(f), f.name, String(f.size || 0), mimeFor(f.name)].join('|'))
    .join('\n');
  try {
    ANDROID.shareFiles(records);
  } catch { toast('Could not share that', 'error'); }
}

/* Files the app cannot preview itself — an APK, a document, an archive —
   should still open. Offer the device's own apps first and downloading second,
   rather than silently saving the file. */
function openUnviewable(file) {
  if (!ANDROID) return downloadFile(file);
  sheet(file.name, [
    { icon: '⧉', label: 'Open with…\nChoose any app installed on this device', run: () => openExternally(file) },
    { icon: '⬇', label: 'Download\nSave to your Downloads folder', run: () => downloadFile(file) },
  ]);
}

function closeViewer() {
  const body = $('#viewer-body');
  // Stop playback and release the connection, or the server keeps streaming.
  for (const el of body.querySelectorAll('video, audio')) {
    el.pause();
    el.removeAttribute('src');
    el.load();
  }
  body.innerHTML = '';
  $('#viewer').hidden = true;
  currentViewFile = null;
}

$('#viewer-close').onclick = closeViewer;
$('#viewer-dl').onclick = () => { const f = currentViewFile; closeViewer(); downloadFile(f); };
$('#viewer-ext').onclick = () => openExternally(currentViewFile);

function fileMenu(file) {
  sheet(file.name, [
    file.isDir
      ? { icon: '📂', label: 'Open', run: () => openFolder(file.accountId, file.id, file.name) }
      : canView(file) && { icon: '▶', label: 'Open', run: () => openViewer(file) },
    !file.isDir && { icon: '⬇', label: 'Download', run: () => downloadFile(file) },
    !file.isDir && ANDROID && { icon: '⧉', label: 'Open in another app', run: () => openExternally(file) },
    !file.isDir && ANDROID && { icon: '📨', label: 'Share', run: () => shareFiles([file]) },
    canShareLink(file) && {
      icon: '🔗',
      label: 'Copy link\nAnyone with it can download this, from any network',
      run: () => shareLink(file),
    },
    { icon: '📤', label: 'Copy to another drive', run: () => chooseTarget([file], false) },
    { icon: '➡', label: 'Move to another drive', run: () => chooseTarget([file], true) },
    { icon: file.starred ? '☆' : '★', label: file.starred ? 'Remove star' : 'Add star', run: async () => {
      try {
        await post('/api/files/star', { account: file.accountId, id: file.id, on: !file.starred });
        loadFiles();
      } catch (err) { fail(err); }
    } },
    { icon: '✎', label: 'Rename', run: () => {
      ask('Rename', [{ label: 'New name', value: file.name }], async ([name]) => {
        if (!name || name === file.name) return;
        try {
          await post('/api/files/rename', { account: file.accountId, id: file.id, name });
          toast('Renamed', 'ok');
          loadFiles();
        } catch (err) { fail(err); }
      });
    } },
    { icon: '🗑', label: 'Delete', danger: true, run: () => deleteFiles([file]) },
  ]);
}

/* ---------------- public links ---------------- */

/* A link has to be minted by the drive itself. OmniDrive listens on this
   phone's loopback address, so a link it served would be unreachable from
   another network and would die the moment the phone slept — the provider's
   own link is served by their CDN and keeps working with the phone off. That
   rules out phone and SD card files, and pooled folders, which have no single
   home drive. */
function canShareLink(file) {
  const acc = state.accounts.find((a) => a.id === file.accountId);
  return !!acc?.capabilities?.share;
}

async function copyText(value) {
  try {
    await navigator.clipboard.writeText(value);
    toast('Link copied', 'ok');
  } catch {
    // Some Android WebViews refuse the clipboard without a user gesture in
    // scope; showing the link at least lets it be copied by hand.
    toast(value);
  }
}

async function shareLink(file) {
  let res;
  try {
    toast('Creating link…');
    res = await post('/api/files/share', {
      account: file.accountId, id: file.id, name: file.name, isDir: !!file.isDir,
    });
  } catch (err) { return fail(err); }

  // Copy straight away: the common case is "give me the link", and the sheet
  // below is for everything else.
  await copyText(res.direct || res.url);
  linkSheet(file, res);
}

function linkSheet(file, res, onDone) {
  const direct = res.direct || res.url;
  const isDir = !!file.isDir;

  sheet(file.name, [
    // Naming the drive matters: with the combined cloud drive on, a file you
    // think of as "in Google Drive" may actually live on pCloud, and the link
    // would otherwise look like it came from nowhere.
    { icon: '🔗', label: `Shared from ${res.drive || 'this drive'}\n${wrapURL(direct)}`, run: () => copyText(direct) },
    {
      icon: '📋',
      label: isDir
        ? 'Copy link\nOpens the folder; they can download what they want'
        : 'Copy download link\nStarts the download for whoever opens it',
      run: () => copyText(direct),
    },
    res.url && res.direct && res.url !== res.direct && {
      icon: '👁',
      label: 'Copy preview link\nOpens a page on the drive instead of downloading',
      run: () => copyText(res.url),
    },
    ANDROID && {
      icon: '📨',
      label: 'Send to an app\nWhatsApp, email, anything installed',
      run: () => {
        try { ANDROID.shareText(direct, file.name); }
        catch { copyText(direct); }
      },
    },
    {
      icon: '⛔',
      label: 'Stop sharing\nThe link stops working for everyone',
      danger: true,
      run: () => revokeLink(file.accountId, file.id, file.name, onDone),
    },
  ]);
}

async function revokeLink(accountId, id, name, onDone) {
  try {
    await post('/api/files/share', { account: accountId, id, revoke: true });
    toast(`"${name}" is private again`, 'ok');
    if (onDone) onDone();
  } catch (err) { fail(err); }
}

/* Long URLs need to be readable in a narrow sheet, and a middle ellipsis keeps
   the host and the tail — the two parts that tell you which link this is. */
function wrapURL(u) {
  if (!u) return '';
  if (u.length <= 52) return u;
  return u.slice(0, 34) + '…' + u.slice(-14);
}

/* Every link handed out, in one place. A public link with no way to find it
   again is a file you have quietly published and forgotten. */
async function openSharedLinks() {
  let data;
  try { data = await api('/api/shares'); }
  catch (err) { return fail(err); }

  const links = data.links || [];
  if (!links.length) {
    return sheet('Shared links', [
      { icon: '🔗', label: 'Nothing is shared\nUse ⋯ on a file, then Copy link', run: () => {} },
    ]);
  }

  const rows = links.map((l) => ({
    icon: l.isDir ? '📁' : '🔗',
    label: `${l.name}\n${l.drive} · shared ${when(l.created)}`,
    run: () => linkSheet(
      { accountId: l.accountId, id: l.fileId, name: l.name, isDir: l.isDir },
      { url: l.url, direct: l.direct, drive: l.drive },
      openSharedLinks,
    ),
  }));

  rows.push({
    icon: '⛔',
    label: `Stop sharing everything\nRevokes all ${links.length} link(s)`,
    danger: true,
    run: () => confirmSheet('Stop sharing all files?', 'Revoke all', async () => {
      let failed = 0;
      for (const l of links) {
        try { await post('/api/files/share', { account: l.accountId, id: l.fileId, revoke: true }); }
        catch { failed++; }
      }
      if (failed) toast(`${failed} link(s) could not be revoked`, 'error');
      else toast('All links revoked', 'ok');
      openSharedLinks();
    }, 'They stop working for everyone immediately.'),
  });

  sheet(`Shared links (${links.length})`, rows);
}

/* ---------------- add menu ---------------- */

$('#fab').onclick = () => {
  const loc = currentLocation();
  if (!loc.account) {
    return sheet('Add', [
      { icon: '☁️', label: 'Connect a drive', run: () => switchView('drives') },
      { icon: '📱', label: 'Add phone storage', run: addLocalStorage },
    ]);
  }
  sheet('Add', [
    { icon: '⬆', label: 'Upload files', run: () => $('#upload-input').click() },
    { icon: '📁', label: 'New folder', run: newFolder },
  ]);
};

function newFolder() {
  const loc = currentLocation();
  if (!loc.account) return toast('Open a drive first', 'error');
  ask('New folder', [{ label: 'Folder name', placeholder: 'Folder name' }], async ([name]) => {
    if (!name) return;
    try {
      await post('/api/files/mkdir', { account: loc.account, parent: loc.folder, name });
      toast('Folder created', 'ok');
      loadFiles();
    } catch (err) { fail(err); }
  });
}

/* ---------------- upload ---------------- */

$('#upload-input').onchange = (e) => {
  const files = Array.from(e.target.files || []);
  e.target.value = '';
  if (!files.length) return;
  const loc = currentLocation();
  files.forEach((f) => uploadOne(f, loc));
};

function uploadOne(file, loc) {
  const q = new URLSearchParams({ name: file.name, size: String(file.size) });
  if (loc.account) q.set('account', loc.account);
  if (loc.folder) q.set('folder', loc.folder);
  if (TOKEN) q.set('t', TOKEN);

  // XHR rather than fetch: it is the only way to observe browser-side upload
  // progress, which is what the user sees first on a slow mobile uplink.
  const xhr = new XMLHttpRequest();
  xhr.open('POST', `/api/upload?${q}`);
  xhr.setRequestHeader('Content-Type', 'application/octet-stream');

  const id = `local-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  state.jobs.set(id, { id, name: file.name, total: file.size, sent: 0, status: 'running' });
  renderTray();

  xhr.upload.onprogress = (ev) => {
    const job = state.jobs.get(id);
    if (job) { job.sent = ev.loaded; renderTray(); }
  };
  xhr.onload = () => {
    const job = state.jobs.get(id);
    if (!job) return;
    job.status = xhr.status >= 200 && xhr.status < 300 ? 'done' : 'error';
    if (job.status === 'error') {
      try { job.error = JSON.parse(xhr.responseText).error; } catch { job.error = xhr.statusText; }
      toast(`${file.name}: ${job.error}`, 'error');
    } else {
      toast(`Uploaded ${file.name}`, 'ok');
      if (state.view === 'files') loadFiles();
    }
    renderTray();
  };
  xhr.onerror = () => {
    const job = state.jobs.get(id);
    if (job) { job.status = 'error'; job.error = 'network error'; renderTray(); }
    toast(`${file.name}: upload failed`, 'error');
  };
  xhr.send(file);
}

function renderTray() {
  // Local XHR jobs track the browser→server leg; the server reports the
  // server→cloud leg for the same file. Counting both would double the bytes
  // and show "2 transfers" for one upload, so server-side *uploads* are left
  // out here. Copies and moves have no local counterpart, so they belong.
  const running = Array.from(state.jobs.values())
    .filter((j) => j.status === 'running')
    .filter((j) => j.local || j.kind !== 'upload');
  const tray = $('#progress-tray');

  if (!running.length) {
    tray.hidden = true;
    for (const [id, j] of state.jobs) {
      if (j.status !== 'running') setTimeout(() => state.jobs.delete(id), 5000);
    }
    renderJobsList();
    return;
  }
  const total = running.reduce((a, j) => a + (j.total || 0), 0);
  const sent = running.reduce((a, j) => a + (j.sent || 0), 0);
  const pct = total > 0 ? Math.round((sent / total) * 100) : 0;

  tray.hidden = false;
  tray.innerHTML = `
    <div class="job-head">
      <span class="job-name">${running.length} transfer${running.length > 1 ? 's' : ''} · ${bytes(sent)} of ${bytes(total)}</span>
      <span class="job-pct">${pct}%</span>
    </div>
    <div class="meter"><span style="width:${pct}%"></span></div>`;
  renderJobsList();
}

function renderJobsList() {
  const box = $('#jobs-list');
  if (!box) return;
  const jobs = Array.from(state.jobs.values());
  if (!jobs.length) { box.innerHTML = '<div class="muted small">No transfers yet.</div>'; return; }

  box.innerHTML = jobs.map((j) => {
    const pct = j.total > 0 ? Math.round((j.sent / j.total) * 100) : (j.status === 'done' ? 100 : 0);
    const right = j.status === 'error' ? (j.error || 'failed') : `${pct}%`;
    return `<div class="job ${j.status}">
      <div class="job-head"><span class="job-name">${escapeHtml(j.name)}</span><span class="job-pct">${escapeHtml(right)}</span></div>
      <div class="meter"><span style="width:${pct}%"></span></div>
    </div>`;
  }).join('');
}

/* ---------------- live events ---------------- */

/* Exactly one event stream at a time.
   Reconnecting used to open a fresh EventSource without closing the previous
   one, and each new stream installed its own error handler that could schedule
   further reconnects. Within a couple of minutes of a flaky link that becomes
   dozens of live connections — each holding a socket here and a goroutine on
   the server — until the app is killed. */

let eventSource = null;
let reconnectTimer = null;
let reconnectDelay = 2000;

function connectEvents() {
  // Never run two streams: close whatever exists before opening another.
  closeEvents();

  const url = TOKEN ? `/api/events?t=${encodeURIComponent(TOKEN)}` : '/api/events';
  const es = new EventSource(url);
  eventSource = es;

  es.addEventListener('open', () => { reconnectDelay = 2000; });

  es.addEventListener('job', (ev) => {
    // A stream we have already replaced must not keep mutating state.
    if (es !== eventSource) return;
    try {
      const job = JSON.parse(ev.data);
      state.jobs.set(job.id, job);
      pruneJobs();
      renderJobsList();
      renderTray();
      if (job.status === 'done' && (job.kind === 'copy' || job.kind === 'move')) {
        toast(`${job.kind === 'move' ? 'Moved' : 'Copied'} to ${job.account}`, 'ok');
        if (state.view === 'files') loadFiles();
      }
    } catch { /* ignore malformed frames */ }
  });

  es.onerror = () => {
    if (es !== eventSource) return;
    // Only step in once the browser has given up; while it is CONNECTING its
    // own retry is already in flight.
    if (es.readyState === EventSource.CLOSED) scheduleReconnect();
  };
}

function closeEvents() {
  if (eventSource) {
    eventSource.onerror = null;
    eventSource.close();
    eventSource = null;
  }
  if (reconnectTimer) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
}

function scheduleReconnect() {
  if (reconnectTimer) return; // one pending attempt, never a queue
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    if (document.hidden) return; // nothing to show; wait until visible again
    connectEvents();
  }, reconnectDelay);
  // Back off so a server that is down does not get hammered.
  reconnectDelay = Math.min(reconnectDelay * 2, 60000);
}

/* Finished jobs accumulate for as long as the app is open. Keep the recent
   ones and drop the rest, or a long session grows without bound. */
function pruneJobs() {
  const MAX = 60;
  if (state.jobs.size <= MAX) return;
  const finished = [...state.jobs.entries()].filter(([, j]) => j.status !== 'running');
  for (const [id] of finished.slice(0, state.jobs.size - MAX)) state.jobs.delete(id);
}

// Android suspends sockets when the screen locks; pick the stream back up when
// the app is in front again rather than leaving it dead.
document.addEventListener('visibilitychange', () => {
  if (document.hidden) return;
  if (!eventSource || eventSource.readyState === EventSource.CLOSED) {
    reconnectDelay = 2000;
    connectEvents();
  }
});


/* ---------------- drives ---------------- */

async function loadDrives() {
  const list = $('#drives-list');
  list.innerHTML = '';

  const toggle = $('#toggle-pool');
  if (toggle) {
    toggle.checked = state.settings ? state.settings.poolEnabled !== false : true;
    toggle.onchange = async () => {
      try {
        state.settings = await api('/api/settings', {
          method: 'PUT', body: JSON.stringify({ poolEnabled: toggle.checked }),
        });
        toast(toggle.checked ? 'Drives combined into one' : 'Drives shown separately', 'ok');
        // The root listing changes shape, so drop any path into the old view.
        state.trail = [];
      } catch (err) { fail(err); toggle.checked = !toggle.checked; }
    };
  }

  try {
    state.accounts = await api('/api/accounts');
    if (!state.accounts.length) {
      list.innerHTML = '<div class="empty">No drives yet. Connect one below, or use the Move tab to bring a setup over from another device.</div>';
      return;
    }
    state.accounts.forEach((a, i) => list.append(driveCard(a, i)));
  } catch (err) { fail(err); }
}

function driveCard(acc, index) {
  const el = document.createElement('div');
  el.className = 'drive';
  const pct = quotaPercent(acc);

  // Everything worth knowing, on tight lines: who it is, which provider, and
  // how full. The identity line carries the account itself — an email for a
  // cloud drive, the folder for device storage.
  const identity = acc.kind === 'local'
    ? (acc.displayName || acc.label)
    : (acc.displayName || (/@/.test(acc.label) ? acc.label : ''));

  const free = acc.quotaTotal > 0
    ? `${bytes(acc.quotaTotal - acc.quotaUsed)} free of ${bytes(acc.quotaTotal)}`
    : (acc.quotaUsed > 0 ? `${bytes(acc.quotaUsed)} used` : 'size unknown');

  el.innerHTML = `
    <div class="drive-head">
      <div class="row-icon">${acc.kind === 'local' ? '📱' : '☁️'}</div>
      <div class="drive-name">${escapeHtml(driveName(acc, index))}</div>
      <span class="badge kind-${escapeHtml(acc.kind)}">${escapeHtml(KIND_LABEL[acc.kind] || acc.kind)}</span>
      ${acc.enabled ? '' : '<span class="badge off">off</span>'}
    </div>
    ${identity ? `<div class="drive-id">${escapeHtml(identity)}</div>` : ''}
    <div class="meter ${pct > 90 ? 'full' : ''}"><span style="width:${pct}%"></span></div>
    <div class="drive-stats"><span>${escapeHtml(quotaText(acc) || '—')}</span><span>${escapeHtml(free)}</span></div>`;

  el.onclick = () => driveMenu(acc, driveName(acc, index));
  return el;
}

function driveMenu(acc, name) {
  if (!acc) return;
  sheet(name, [
    { icon: '📂', label: 'Browse', run: () => {
      state.trail = [{ accountId: acc.id, folderId: '', name }];
      switchView('files');
    } },
    acc.capabilities?.browseTrash && {
      icon: '♻',
      label: 'Recycle bin\nRestore anything deleted from this drive',
      run: () => openRecycleBin(acc),
    },
    // Cloud drives delete outright, so their bin only holds what was removed
    // before this app, or from the provider's own site.
    !acc.capabilities?.browseTrash && acc.capabilities?.emptyTrash && {
      icon: '♻',
      label: 'Empty Trash\nReclaims space still held by earlier deletions',
      run: () => emptyTrash(acc),
    },
    // Cookie-signed-in drives have no refresh token, so "sign in again" is the
    // whole repair procedure when one lapses — and it has to be findable
    // without disconnecting the drive and losing its name and settings first.
    usesWebLogin(acc.kind) && {
      icon: '🔑',
      label: `Sign in to ${KIND_LABEL[acc.kind] || acc.kind} again\nUse this if it says the sign-in expired`,
      run: async () => {
        const p = await providerFor(acc.kind);
        if (p) connectWebLogin(p);
      },
    },
    { icon: '✎', label: 'Rename this drive', run: () => {
      ask('Rename drive', [{ label: 'Name', value: acc.label }], async ([label]) => {
        if (!label) return;
        try {
          await api(`/api/accounts/${acc.id}`, { method: 'PATCH', body: JSON.stringify({ label }) });
          state.accounts = await api('/api/accounts');
          switchView(state.view);
        } catch (err) { fail(err); }
      });
    } },
    { icon: acc.enabled ? '⏸' : '▶', label: acc.enabled ? 'Disable' : 'Enable', run: async () => {
      try {
        await api(`/api/accounts/${acc.id}`, { method: 'PATCH', body: JSON.stringify({ enabled: !acc.enabled }) });
        loadDrives();
      } catch (err) { fail(err); }
    } },
    { icon: '🗑',
      label: usesWebLogin(acc.kind) ? 'Sign out and disconnect' : 'Disconnect',
      danger: true,
      run: () => {
        confirmSheet(`Disconnect ${name}?`, 'Disconnect', async () => {
          try {
            await api(`/api/accounts/${acc.id}`, { method: 'DELETE' });
            // Dropping the account alone would leave the sign-in page still
            // signed in, so the next "connect" would silently reuse the same
            // session — which is not what anyone means by signing out.
            if (usesWebLogin(acc.kind) && ANDROID && ANDROID.clearWebLogin) {
              try { ANDROID.clearWebLogin(acc.kind); } catch { /* best effort */ }
            }
            state.accounts = await api('/api/accounts');
            toast('Disconnected', 'ok');
            loadDrives();
          } catch (err) { fail(err); }
        });
      } },
  ]);
}

/* Kinds that sign in on the provider's own web page rather than through OAuth
   or a credentials form. Read from the provider catalogue where it is loaded,
   with the one known case as the fallback so the Drives tab works before it. */
function usesWebLogin(kind) {
  const p = state.providers.find((x) => x.kind === kind);
  return p ? p.auth === 'webview' : kind === 'terabox';
}

async function providerFor(kind) {
  try {
    if (!state.providers.length) state.providers = await api('/api/providers');
  } catch (err) { fail(err); return null; }
  return state.providers.find((p) => p.kind === kind) || null;
}

$('#btn-add-storage').onclick = addLocalStorage;
$('#btn-shared-links').onclick = openSharedLinks;

/* Offer the volumes this device actually has rather than asking the user to
   type "/storage/emulated/0" from memory. */
async function addLocalStorage() {
  if (ANDROID && !ANDROID.hasAllFilesAccess()) {
    return sheet('Permission needed', [
      { icon: '🔓', label: 'Grant "All files access"\nAndroid requires this before any app can browse your whole storage.',
        run: () => { ANDROID.requestAllFilesAccess(); toast('Turn OmniDrive on, then come back'); } },
      { icon: '↩', label: 'Not now', run: () => {} },
    ]);
  }
  let data;
  try { data = await api('/api/storage'); } catch (err) { return fail(err); }

  // Show unreadable volumes too, with a way to fix them. Hiding everything
  // and showing only an error left no route forward.
  const volumes = data.volumes || [];
  if (!volumes.length) return toast('No storage volumes found', 'error');

  const items = volumes.map((v) => ({
    icon: v.readable ? (v.primary ? '📱' : '💾') : '🔒',
    label: v.readable
      ? `${v.label}\n${v.path} · ${v.entries} items`
      : `${v.label} — no access\n${v.path} · grant "All files access" to use this`,
    run: async () => {
      if (!v.readable) {
        if (ANDROID) { ANDROID.requestAllFilesAccess(); toast('Turn OmniDrive on, then come back'); }
        else toast('This folder is not readable', 'error');
        return;
      }
      try {
        await post('/api/storage/add', { path: v.path, label: v.label });
        toast(`Added ${v.label}`, 'ok');
        state.accounts = await api('/api/accounts');
        loadDrives();
      } catch (err) { fail(err); }
    },
  }));

  if (data.needsAccess && ANDROID) {
    items.unshift({
      icon: '🔓',
      label: 'Grant "All files access"\nAndroid requires this before any app can browse your storage.',
      run: () => { ANDROID.requestAllFilesAccess(); toast('Turn OmniDrive on, then come back'); },
    });
  }
  sheet('Add storage', items);
}

/* ---------------- view modes ---------------- */

/* Explorer-style layouts for the file list. The choice is remembered per
   device rather than per account: it is a preference about your eyes, not
   about the data. */

const VIEW_MODES = [
  ['list', 'List', 'Name and details on one line'],
  ['compact', 'Compact', 'Tighter rows, more on screen'],
  ['grid', 'Large icons', 'Tiles with big thumbnails'],
  ['details', 'Details', 'Name, size, type and date'],
];

function currentViewMode() {
  return localStorage.getItem('omnidrive.viewmode') || 'list';
}

function applyViewMode(mode) {
  localStorage.setItem('omnidrive.viewmode', mode);
  const list = $('#files-list');
  for (const [key] of VIEW_MODES) list.classList.remove(`view-${key}`);
  list.classList.add(`view-${mode}`);
}

function showViewModes() {
  const active = currentViewMode();
  sheet('View', VIEW_MODES.map(([key, label, detail]) => ({
    icon: key === active ? '●' : '○',
    label: `${label}\n${detail}`,
    run: () => { applyViewMode(key); renderCurrent(); },
  })));
}

/* One overflow button instead of a row of icons. Five glyphs across the top of
   every screen is noise; sort, view, select and refresh are all occasional. */
$('#btn-more').onclick = () => {
  sheet('Options', [
    { icon: '⇅', label: 'Sort', run: showSortSheet },
    { icon: '▤', label: 'View', run: showViewModes },
    { icon: '☑', label: 'Select files', run: () => enterSelectMode() },
    { icon: '⟳', label: 'Refresh', run: () => switchView(state.view) },
  ]);
};

/* ---------------- setup guide ---------------- */

/* A walkthrough inside the app, so connecting a drive never means hunting for
   documentation on a phone. The steps come from the server alongside the
   provider definitions, which keeps them from drifting out of date. */

const guide = { provider: null };

function redirectURI() { return `${location.origin}/oauth/callback`; }

function openGuide(p) {
  guide.provider = p || null;
  $('#guide').hidden = false;
  syncBackGuard();
  renderGuide();
}

function closeGuide() { $('#guide').hidden = true; guide.provider = null; }

$('#guide-close').onclick = closeGuide;
$('#guide-back').onclick = () => {
  if (guide.provider) { guide.provider = null; renderGuide(); }
  else closeGuide();
};

const EFFORT_TEXT = {
  none: 'No setup',
  easy: 'Quick setup',
  console: 'Developer console',
};

async function renderGuide() {
  const body = $('#guide-body');
  const actions = $('#guide-actions');
  body.innerHTML = '';

  if (!state.providers.length) {
    try { state.providers = await api('/api/providers'); } catch (err) { return fail(err); }
  }

  // Provider chooser.
  if (!guide.provider) {
    $('#guide-title').textContent = 'How to connect a drive';
    actions.hidden = true;

    const intro = document.createElement('p');
    intro.className = 'guide-intro';
    intro.textContent = 'Pick a provider to see the exact steps. The ones marked '
      + '"No setup" need nothing but a username and password.';
    body.append(intro);

    // Least effort first: someone new should meet the easy options first.
    const order = { none: 0, easy: 1, console: 2 };
    const sorted = [...state.providers].sort(
      (a, b) => (order[a.effort] ?? 3) - (order[b.effort] ?? 3));

    for (const p of sorted) {
      const row = document.createElement('div');
      row.className = 'guide-pick';
      row.innerHTML = `
        <div class="row-icon">${p.kind === 'local' ? '📱' : '☁️'}</div>
        <div class="guide-pick-body">
          <div class="guide-pick-name">${escapeHtml(p.label)}</div>
          <div class="guide-pick-sub">${escapeHtml(p.free ? `Free: ${p.free}` : (p.notes || '').slice(0, 60))}</div>
        </div>
        <span class="effort ${p.effort || 'easy'}">${EFFORT_TEXT[p.effort] || 'Setup'}</span>`;
      row.onclick = () => { guide.provider = p; renderGuide(); };
      body.append(row);
    }
    return;
  }

  // Steps for one provider.
  const p = guide.provider;
  $('#guide-title').textContent = p.label;
  actions.hidden = false;
  $('#guide-connect').onclick = () => {
    closeGuide();
    startConnect(p);
  };

  if (p.free) {
    const intro = document.createElement('p');
    intro.className = 'guide-intro';
    intro.textContent = `Free allowance: ${p.free}`;
    body.append(intro);
  }

  (p.setup || []).forEach((step, i) => {
    const el = document.createElement('div');
    el.className = 'guide-step';

    const num = document.createElement('div');
    num.className = 'guide-num';
    num.textContent = String(i + 1);
    el.append(num);

    const text = document.createElement('div');
    text.className = 'guide-text';
    text.append(document.createTextNode(step.text));

    if (step.warn) {
      const warn = document.createElement('span');
      warn.className = 'guide-warn';
      warn.textContent = step.warn;
      text.append(warn);
    }
    if (step.copy) {
      // The redirect URI depends on the port actually in use, so it is
      // substituted here rather than baked into the guide.
      const value = step.copy.replace('{{redirect}}', redirectURI());
      const box = document.createElement('div');
      box.className = 'guide-copy';
      box.innerHTML = `<span>${escapeHtml(value)}</span>📋`;
      box.onclick = async () => {
        try { await navigator.clipboard.writeText(value); toast('Copied', 'ok'); }
        catch { toast(value); }
      };
      text.append(box);
    }
    if (step.link) {
      const a = document.createElement('a');
      a.className = 'guide-link';
      a.href = step.link;
      a.target = '_blank';
      a.rel = 'noreferrer';
      a.textContent = 'Open this page ↗';
      a.onclick = (e) => {
        // openUrl hands it to the real browser. openExternal is for files and
        // takes four arguments, so the old two-argument call matched no method
        // at all and the link silently did nothing on Android.
        if (!ANDROID || !ANDROID.openUrl) return;
        e.preventDefault();
        ANDROID.openUrl(step.link);
      };
      text.append(a);
    }
    el.append(text);
    body.append(el);
  });
}

$('#btn-guide').onclick = () => openGuide(null);

/* The recycle bin for phone and SD card storage. Cloud deletes are final, so
   only local drives ever reach this screen. */
async function openRecycleBin(acc) {
  const name = driveName(acc, accountIndex(acc.id));
  let data;
  try {
    toast('Opening recycle bin…');
    data = await api(`/api/accounts/${acc.id}/trash`);
  } catch (err) { return fail(err); }

  const items = data.items || [];
  if (!items.length) {
    sheet(`Recycle bin — ${name}`, [
      { icon: '✓', label: 'Empty\nNothing has been deleted from this drive', run: () => {} },
    ]);
    return;
  }

  // Newest first, and cap the list: a sheet with 400 rows is unusable, and the
  // "empty everything" action below still covers the rest.
  const shown = items.slice(0, 50);
  const rows = shown.map((it) => ({
    icon: it.isDir ? '📁' : '📄',
    label: `${it.name}\n${it.isDir ? 'folder' : bytes(it.size)} · deleted ${when(it.deleted)}`,
    run: () => binItemMenu(acc, it, name),
  }));

  if (items.length > shown.length) {
    rows.push({ icon: '…', label: `${items.length - shown.length} more not shown`, run: () => {} });
  }
  rows.push({
    icon: '⚠',
    label: `Empty the bin\nDestroys all ${items.length} item(s) and frees ${bytes(data.size || 0)}`,
    danger: true,
    run: () => emptyTrash(acc),
  });
  sheet(`Recycle bin — ${name}`, rows);
}

function binItemMenu(acc, item, driveLabel) {
  const back = () => openRecycleBin(acc);
  sheet(item.name, [
    {
      icon: '↩',
      label: item.originalPath
        ? `Restore\nBack to ${item.originalPath}`
        : 'Restore\nThe original folder is unknown, so it goes to the top level',
      run: async () => {
        try {
          const res = await post(`/api/accounts/${acc.id}/trash/restore`, { ids: [item.id] });
          const bad = (res.results || []).find((r) => !r.ok);
          if (bad) return fail(new Error(bad.error || 'restore failed'));
          toast('Restored', 'ok');
          loadFiles();
          back();
        } catch (err) { fail(err); }
      },
    },
    {
      icon: '⚠',
      label: 'Delete forever\nFrees the space now. Cannot be undone',
      danger: true,
      run: () => confirmSheet(`Delete "${item.name}" forever?`, 'Delete forever', async () => {
        try {
          const res = await post(`/api/accounts/${acc.id}/trash/purge`, { ids: [item.id] });
          const bad = (res.results || []).find((r) => !r.ok);
          if (bad) return fail(new Error(bad.error || 'delete failed'));
          toast('Deleted', 'ok');
          loadDrives();
          back();
        } catch (err) { fail(err); }
      }),
    },
    { icon: '←', label: `Back to ${driveLabel} bin`, run: back },
  ]);
}

/* Emptying the recycle bin is the only way to reclaim space on providers that
   trash rather than delete — without it a user can delete everything in the
   app and still be full. */
function emptyTrash(acc) {
  sheet(`Empty Trash on ${driveName(acc, accountIndex(acc.id))}?`, [
    {
      icon: '⚠',
      label: 'Empty Trash\nPermanently discards everything already deleted',
      danger: true,
      run: async () => {
        try {
          toast('Emptying Trash…');
          const res = await post(`/api/accounts/${acc.id}/trash/empty`, {});
          toast(`Trash emptied on ${res.drive}`, 'ok');
          await api('/api/accounts/refresh', { method: 'POST' });
          loadDrives();
        } catch (err) { fail(err); }
      },
    },
    { icon: '↩', label: 'Cancel', run: () => {} },
  ]);
}

$('#btn-add-drive').onclick = async () => {
  try {
    if (!state.providers.length) state.providers = await api('/api/providers');
  } catch (err) { return fail(err); }

  sheet('Connect a drive', state.providers.map((p) => {
    const ready = state.settings?.oauthConfigured?.[p.kind];
    let note = ' · no setup needed';
    if (p.auth === 'oauth') note = ready ? ' · ready' : ' · needs a client ID once';
    if (p.auth === 'path') note = ' · this device';
    if (p.auth === 'webview') note = p.free ? ` · sign in · ${p.free} free` : ' · sign in';
    return {
      icon: p.kind === 'local' ? '📱' : '☁️',
      label: p.label + note,
      run: () => startConnect(p),
    };
  }));
};

/* One door for every provider, so the guide, the add sheet and any future
   entry point all reach the same flow. */
function startConnect(p) {
  if (p.auth === 'oauth') return connectOAuth(p);
  if (p.auth === 'path') return addLocalStorage();
  if (p.auth === 'webview') return connectWebLogin(p);
  return connectDirect(p);
}

/* Providers with no OAuth at all — TeraBox is the one — sign in on their own
   web page and hand back a session cookie. Inside the Android app that page can
   be opened in a WebView we own, so the cookie never has to be seen by the user;
   in a desktop browser there is no such page to open, and pasting it by hand is
   the only route left. */
function connectWebLogin(p) {
  const canWebLogin = !!(ANDROID && ANDROID.webLogin);

  const manual = () => connectDirect(p);
  const options = [];

  if (canWebLogin) {
    options.push({
      icon: '→',
      label: `Sign in to ${p.label}\nOpens the ${p.label} sign-in page. Nothing but the session is kept.`,
      run: () => {
        try {
          ANDROID.webLogin(p.kind, p.label);
        } catch (err) { fail(err); }
      },
    });
  }
  options.push({
    icon: '📋',
    label: canWebLogin ? 'Paste a cookie instead\nFor a session copied from a desktop browser'
      : `Paste the ndus cookie\nSign in at ${p.label} in this browser first, then copy it from developer tools`,
    run: manual,
  });
  options.push({ icon: '❔', label: 'Show the steps', run: () => openGuide(p) });

  sheet(p.label, options);
}

/* Called by the Android shell once its sign-in WebView has captured a session.
   It is a plain function on window rather than a fetch from the Java side so
   that the page can refresh itself the moment the drive appears. */
window.omniWebLoginDone = async (kind, ok, message) => {
  if (!ok) {
    toast(message || 'Sign-in was cancelled');
    return;
  }
  try {
    state.accounts = await api('/api/accounts');
    state.providers = state.providers.length ? state.providers : await api('/api/providers');
    toast(`${KIND_LABEL[kind] || kind} connected`, 'ok');
    loadDrives();
  } catch (err) { fail(err); }
};

async function connectOAuth(p) {
  const configured = state.settings?.oauthConfigured?.[p.kind];
  const stored = state.settings?.oauthStored?.[p.kind];
  const redirect = `${location.origin}/oauth/callback`;

  const go = async (clientId, clientSecret, scopeMode) => {
    try {
      const res = await post('/api/connect/oauth/start',
        { kind: p.kind, clientId, clientSecret, scopeMode });
      location.href = res.authUrl;
    } catch (err) { fail(err); }
  };

  // Providers offering several access levels ask first — the choice decides
  // whether the connection needs review and whether it expires.
  const start = (clientId = '', clientSecret = '') => {
    if (!p.scopes || p.scopes.length < 2) return go(clientId, clientSecret, '');
    sheet(`${p.label} — how much access?`, p.scopes.map((s) => ({
      icon: s.default ? '✓' : '⚠',
      label: `${s.label}\n${s.detail}`,
      run: () => go(clientId, clientSecret, s.mode),
    })));
  };

  const enterCredentials = () => {
    ask(`${p.label} credentials`, [
      { label: 'Client ID', placeholder: 'Client ID' },
      { label: 'Client secret', placeholder: 'Client secret (blank if none)' },
    ], ([id, secret]) => {
      if (!id) return toast('Client ID is required', 'error');
      start(id, secret);
    });
  };

  const copyRedirect = async () => {
    try { await navigator.clipboard.writeText(redirect); toast('Copied: ' + redirect, 'ok'); }
    catch { toast(redirect); }
  };

  const clearCredentials = async () => {
    try {
      const res = await api(`/api/settings/oauth/${p.kind}`, { method: 'DELETE' });
      state.settings = await api('/api/settings');
      toast(res.fallsBackToBuiltin
        ? 'Cleared — falling back to the built-in client ID'
        : 'Cleared — you can enter a new client ID', 'ok');
    } catch (err) { fail(err); }
  };

  // Already set up: sign in, but always leave a way out. A client ID is saved
  // before the sign-in that would prove it works, so a typo must be fixable.
  if (configured) {
    return sheet(p.label, [
      { icon: '→', label: 'Sign in', run: () => start() },
      { icon: '✎', label: 'Use a different client ID', run: enterCredentials },
      stored && { icon: '🗑', label: `Clear saved credentials (${stored})`, danger: true, run: clearCredentials },
      { icon: '📋', label: 'Copy redirect URI', run: copyRedirect },
    ]);
  }

  sheet(`${p.label} — one-time setup`, [
    { icon: '🔗', label: 'Open developer console', run: () => window.open(p.oauthDoc, '_blank') },
    { icon: '📋', label: `Copy redirect URI — ${redirect}`, run: copyRedirect },
    { icon: '🔑', label: 'Enter client ID and secret', run: enterCredentials },
  ]);
  if (p.notes) toast(p.notes);
}

function connectDirect(p) {
  if (p.notes) toast(p.notes);
  ask(p.label, p.fields.map((f) => ({
    label: f.label,
    // The hint carries the detail that actually trips people up, such as
    // WebDAV needing an app password rather than the login password.
    placeholder: f.hint || (f.label + (f.required ? '' : ' (optional)')),
    type: f.type === 'password' ? 'password' : 'text',
    value: p.presets?.[0]?.values?.[f.key] || '',
  })), async (values) => {
    const fields = {};
    p.fields.forEach((f, i) => { fields[f.key] = values[i]; });
    try {
      toast('Checking credentials…');
      await post('/api/connect/direct', { kind: p.kind, fields });
      toast('Connected', 'ok');
      state.accounts = await api('/api/accounts');
      loadDrives();
    } catch (err) { fail(err); }
  });
}

/* ---------------- move setup ---------------- */

async function loadSync() {
  try { state.accounts = await api('/api/accounts'); } catch (err) { return fail(err); }

  const sel = $('#cloud-account');
  const cloud = state.accounts.filter((a) => a.kind !== 'local');
  sel.innerHTML = cloud.length
    ? cloud.map((a, i) => `<option value="${a.id}">${escapeHtml(driveName(a, accountIndex(a.id)))}</option>`).join('')
    : '<option value="">Connect a cloud drive first</option>';

  const preferred = state.settings?.cloudSyncAccount;
  if (preferred && cloud.some((a) => a.id === preferred)) sel.value = preferred;
  refreshCloudStatus();
}

async function refreshCloudStatus() {
  const id = $('#cloud-account').value;
  const box = $('#cloud-status');
  if (!id) { box.textContent = ''; return; }
  box.textContent = 'Checking…';
  try {
    const s = await api(`/api/portable/cloud/status?account=${encodeURIComponent(id)}`);
    box.textContent = s.exists
      ? `Saved copy found · ${bytes(s.size)} · ${when(s.modified) || 'recently'}`
      : 'No saved copy in this drive yet.';
  } catch (err) { box.textContent = err.message; }
}

$('#cloud-account').onchange = refreshCloudStatus;

let pairLink = '';

$('#btn-pair-start').onclick = async () => {
  try {
    const offer = await post('/api/portable/pair/start', {});
    // One link carrying the code, so there is a single thing to pass along.
    pairLink = offer.link;
    $('#pair-offer').hidden = false;
    $('#pair-link').textContent = pairLink;
    toast(`Sharing ${offer.accounts} drive${offer.accounts > 1 ? 's' : ''}`, 'ok');
  } catch (err) { fail(err); }
};

$('#btn-pair-copy').onclick = async () => {
  if (!pairLink) return;
  try {
    await navigator.clipboard.writeText(pairLink);
    toast('Link copied — paste it on the other device', 'ok');
  } catch {
    // Clipboard access can be refused; select the text so it can be copied
    // by hand rather than leaving the user stuck.
    const range = document.createRange();
    range.selectNodeContents($('#pair-link'));
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    toast('Copy the highlighted link');
  }
};

$('#btn-pair-join').onclick = async () => {
  const url = $('#pair-join-url').value.trim();
  if (!url) return toast('Paste the pairing link first', 'error');
  try {
    toast('Contacting the other device…');
    const res = await post('/api/portable/pair/join', { url, replace: false });
    toast(`Received ${res.added} new and ${res.updated} updated drive(s)`, 'ok');
    $('#pair-join-url').value = '';
    loadSync();
  } catch (err) { fail(err); }
};

$('#btn-cloud-push').onclick = async () => {
  const account = $('#cloud-account').value;
  if (!account) return toast('Connect a drive first', 'error');
  try {
    toast('Uploading setup…');
    const res = await post('/api/portable/cloud/push', { account, passphrase: '' });
    toast(`Saved ${bytes(res.size)} to ${res.account}`, 'ok');
    refreshCloudStatus();
  } catch (err) { fail(err); }
};

$('#btn-cloud-pull').onclick = async () => {
  const account = $('#cloud-account').value;
  if (!account) return toast('Connect a drive first', 'error');
  try {
    toast('Restoring…');
    const res = await post('/api/portable/cloud/pull', { account, passphrase: '', replace: false });
    toast(`Restored: ${res.added} new, ${res.updated} updated`, 'ok');
    loadSync();
  } catch (err) { fail(err); }
};

$('#btn-export').onclick = async () => {
  const name = 'omnidrive-config.omnibundle';
  const url = `${location.origin}/api/portable/export${TOKEN ? `?t=${encodeURIComponent(TOKEN)}` : ''}`;

  // A plain URL, not a blob: the Android side re-fetches what it is given, and
  // a blob URL belongs to this page and cannot be requested again.
  if (ANDROID && ANDROID.download) {
    ANDROID.download(url, name, 'application/octet-stream');
    toast('Saving to Downloads…');
    return;
  }
  try {
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    a.click();
    toast('Bundle downloaded', 'ok');
  } catch (err) { fail(err); }
};

$('#import-file').onchange = async (e) => {
  const file = e.target.files?.[0];
  e.target.value = '';
  if (!file) return;
  importBundleFile(file, '');
};

async function importBundleFile(file, passphrase) {
  const fd = new FormData();
  fd.append('bundle', file);
  fd.append('passphrase', passphrase);
  fd.append('replace', 'false');
  try {
    const res = await api('/api/portable/import', { method: 'POST', body: fd });
    toast(`Imported: ${res.added} new, ${res.updated} updated`, 'ok');
    loadSync();
  } catch (err) {
    // Bundles exported with a passphrase still import — just ask for it then,
    // rather than making everyone type one every time.
    if (/passphrase-protected/i.test(err.message)) {
      ask('This bundle is protected', [
        { label: 'Passphrase', placeholder: 'Passphrase', type: 'password' },
      ], ([pass]) => { if (pass) importBundleFile(file, pass); });
      return;
    }
    fail(err);
  }
}

/* ---------------- settings ---------------- */

const STRATEGIES = [
  ['most_free', 'Drive with the most free space'],
  ['least_used', 'Drive with the least used'],
  ['round_robin', 'Round robin'],
  ['weighted_round_robin', 'Weighted round robin'],
  ['manual', 'Manual priority order'],
];

async function loadSettings() {
  try { state.settings = await api('/api/settings'); } catch (err) { return fail(err); }

  const sel = $('#strategy');
  sel.innerHTML = STRATEGIES.map(([v, l]) => `<option value="${v}">${l}</option>`).join('');
  sel.value = state.settings.strategy;
  sel.onchange = async () => {
    try {
      state.settings = await api('/api/settings', {
        method: 'PUT', body: JSON.stringify({ strategy: sel.value }),
      });
      toast('Saved', 'ok');
    } catch (err) { fail(err); }
  };

  try {
    const h = await api('/api/health');
    $('#about').innerHTML = `
      OmniDrive ${escapeHtml(h.version)}<br>
      ${h.accounts} drive(s) connected<br>
      Data: <code>${escapeHtml(h.dataDir)}</code><br>
      Certificates: ${h.rootCerts || 0} from ${escapeHtml(h.certSource || 'system')}<br>
      ${h.dnsPatched ? `Android DNS fix active (${escapeHtml((h.dnsServers || []).slice(0, 2).join(', '))})` : 'System DNS'}`;
  } catch { /* the about box is cosmetic */ }

  renderJobsList();
}

/* ---------------- boot ---------------- */

(async function boot() {
  applyViewMode(currentViewMode());
  try {
    state.settings = await api('/api/settings');
    state.accounts = await api('/api/accounts');
  } catch (err) {
    fail(err);
  }
  connectEvents();
  switchView('files');
})();
