// App card (design spec "Tools sheet" section 1, client Task 9). Shows the
// foreground app: a letter tile standing in for the icon the spec defers
// ("Out of scope"), a readable name over the package, the screen and
// version, and the developer fields (activity, pid, APK path) folded under
// Details.
//
// The real server response has no `label` field. The plan's App shape
// carries package + label + icon; internal/target/app.go's actual App struct
// is Package/Activity/VersionName/VersionCode/PID/APKPath, with a comment
// saying the icon is deferred and pid/version/path are "best effort" -- so
// the title is the package's last segment until a `label` arrives, and
// formatApp uses one the moment the server sends it.
//
// GET /api/app answers 501 for a target with no AppInfo (Synthetic today),
// which hides the section the same way sheetHandle.hideSection already does
// for App/Event log/Device before this task -- a missing section rather than
// an empty one.
//
// Split the way inspect.js is: pure functions with no DOM, testable against
// plain objects, plus mount() for the DOM half (app.js's own pattern; no
// harness here, trap 21).

import { showToast } from './toast.js';

// initials(name) -> the 1-2 letters the letter tile shows. Takes the last
// dotted segment (the part a person actually recognises, e.g. "settings" out
// of "com.android.settings"; a display name like "Settings" has no dots and
// is its own last segment) and uppercases the first couple of its letters.
// Falls back to "?" for anything that yields none -- an empty string, or a
// package that is nothing but dots and digits.
export function initials(name) {
  if (!name || typeof name !== 'string') return '?';
  const parts = name.split('.').filter(Boolean);
  const last = parts.length ? parts[parts.length - 1] : name;
  const letters = last.replace(/[^A-Za-z]/g, '');
  if (!letters) return '?';
  return letters.slice(0, 2).toUpperCase();
}

// displayName(pkg) -> a readable stand-in for the app's label, which the
// server does not send yet: the package's last dotted segment with its first
// letter raised, "Settings" for com.android.settings and "Chrome" for
// com.android.chrome. A segment with no letters gives the package back
// unchanged rather than an empty title.
export function displayName(pkg) {
  if (!pkg || typeof pkg !== 'string') return '';
  const parts = pkg.split('.').filter(Boolean);
  const last = parts.length ? parts[parts.length - 1] : pkg;
  if (!/[A-Za-z]/.test(last)) return pkg;
  return last.charAt(0).toUpperCase() + last.slice(1);
}

// screenName(activity) -> the activity's class name without its package:
// ".Settings" and "com.android.settings.Settings" both read "Settings", and
// "com.google.android.apps.chrome.Main" reads "Main". A nested class keeps
// its "$Inner" suffix, since that is often the part that says which screen.
export function screenName(activity) {
  if (!activity || typeof activity !== 'string') return '';
  const parts = activity.split('.').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : activity;
}

// formatApp(app) -> {title, subtitle, rows, details} | null
//
// app is the raw GET /api/app body (the App struct's own JSON -- there is no
// wrapper, see internal/httpapi/app.go's handleApp) or null/undefined before
// the first fetch. Only Package is guaranteed; every other field is filled
// "on a best-effort basis" per app.go's own comment, so a zero/empty one is
// left out rather than rendered as "0" or "".
//
// title is the app's label when the body carries one, else displayName; the
// package is the subtitle under it. rows are what a person reads at a
// glance (the screen and the version, the name and code as one row); details
// are the developer fields -- the full activity, PID and APK path -- which
// the card keeps folded, because a FQCN or an /data/app/~~... path is what
// made the card overflow a 360px panel.
export function formatApp(app) {
  if (!app || !app.package) return null;
  const rows = [];
  const details = [];
  if (app.activity) {
    rows.push({ label: 'Screen', value: screenName(app.activity) });
    details.push({ label: 'Activity', value: app.activity });
  }
  if (app.versionName && app.versionCode) {
    rows.push({ label: 'Version', value: `${app.versionName} (${app.versionCode})` });
  } else if (app.versionName) {
    rows.push({ label: 'Version', value: app.versionName });
  } else if (app.versionCode) {
    rows.push({ label: 'Version', value: `(${app.versionCode})` });
  }
  if (app.pid) details.push({ label: 'PID', value: String(app.pid) });
  if (app.apkPath) details.push({ label: 'APK', value: app.apkPath });
  const title = (typeof app.label === 'string' && app.label) || displayName(app.package);
  return { title, subtitle: app.package, rows, details };
}

// shouldRefetch(lastFetchAt, now, minIntervalMs = 2000) -> bool
//
// Rate-limits a refetch triggered by device activity to at most once per
// minIntervalMs (design spec: "/api/app" is fetched "when the App section is
// open, and again after a MsgEvent, rate-limited"). lastFetchAt is
// null/undefined before the first fetch, which always passes.
export function shouldRefetch(lastFetchAt, now, minIntervalMs = 2000) {
  if (lastFetchAt == null) return true;
  return now - lastFetchAt >= minIntervalMs;
}

// errorMessage(res) -> the server's `error` string from its JSON body
// ({"error": "..."} -- every agent-route failure in internal/httpapi/agent.go
// is shaped this way), or a plain status line when the body isn't JSON.
async function errorMessage(res) {
  let body = null;
  try { body = await res.json(); } catch { /* not JSON; fall through */ }
  if (body && typeof body.error === 'string') return body.error;
  return `app: ${res.status}`;
}

// mount(container, deps) builds the App card into `container` (the sheet's
// 'app' section content element) and returns {refresh(), notifyActivity()}.
// Whether the section exists at all is the caller's call, via
// sheetHandle.hideSection/showSection -- this module only decides what to
// draw once, or asks to be hidden through deps.onUnavailable() and shown
// again through deps.onAvailable() once a fetch for the CURRENT target
// actually succeeds.
//
// deps:
//   targetId() -- the currently selected target id.
//   fetchImpl -- defaults to window.fetch.
//   onUnavailable() -- called every time GET /api/app answers 501.
//   onAvailable() -- called once a fetch for the target in hand succeeds; a
//     switch away from a 501 target must not leave the section hidden
//     forever (review HIGH-1).
//   report(rec) -- app.js's report(), for a failure that is not a 501.
export function mount(container, deps = {}) {
  let lastFetchAt = null;
  let unavailable = false;
  let lastTargetId; // undefined sentinel: "no fetch attempted yet"
  // gen guards a response that arrives after a later switch already
  // rendered for a different target (trap 41) -- a slow fetch for A must
  // not paint over B just because it happened to resolve after B's own,
  // faster fetch already landed.
  let gen = 0;

  function rowsList(rows, className) {
    const dl = document.createElement('dl');
    // appRows (on top of the shared sheetRows grid) is what keeps a long
    // activity or path value left-aligned and wrapping inside the panel
    // instead of forcing the row wider -- see style.css's own comment on
    // both classes.
    dl.className = className;
    for (const row of rows) {
      const dt = document.createElement('dt');
      dt.textContent = row.label;
      const dd = document.createElement('dd');
      dd.textContent = row.value;
      dd.title = row.value;
      dl.appendChild(dt);
      dl.appendChild(dd);
    }
    return dl;
  }

  function render(app) {
    container.innerHTML = '';
    const formatted = formatApp(app);
    if (!formatted) return;
    const card = document.createElement('div');
    card.className = 'appCard';

    const head = document.createElement('div');
    head.className = 'appHead';
    const tile = document.createElement('div');
    tile.className = 'appTile';
    tile.textContent = initials(formatted.title);
    tile.setAttribute('aria-hidden', 'true');
    const names = document.createElement('div');
    names.className = 'appNames';
    const title = document.createElement('div');
    title.className = 'appTitle';
    title.textContent = formatted.title;
    title.title = formatted.title;
    const subtitle = document.createElement('div');
    subtitle.className = 'appSubtitle';
    subtitle.textContent = formatted.subtitle;
    subtitle.title = formatted.subtitle;
    names.appendChild(title);
    names.appendChild(subtitle);
    head.appendChild(tile);
    head.appendChild(names);
    card.appendChild(head);

    if (formatted.rows.length) card.appendChild(rowsList(formatted.rows, 'sheetRows appRows'));
    if (formatted.details.length) {
      const more = document.createElement('details');
      more.className = 'appDetails';
      const summary = document.createElement('summary');
      summary.textContent = 'Details';
      more.appendChild(summary);
      more.appendChild(rowsList(formatted.details, 'sheetRows appRows appRows--stacked'));
      card.appendChild(more);
    }
    container.appendChild(card);
  }

  async function refresh() {
    const targetId = deps.targetId ? deps.targetId() : null;
    if (targetId !== lastTargetId) {
      // A fresh target: whatever the PREVIOUS target answered about 501 says
      // nothing about this one, so it gets its own attempt rather than
      // inheriting a latch that belongs to a target nobody is watching
      // anymore (review HIGH-1).
      lastTargetId = targetId;
      unavailable = false;
    }
    if (unavailable) return;
    const fetchImpl = deps.fetchImpl || (typeof fetch !== 'undefined' ? fetch : null);
    if (!fetchImpl || !deps.targetId) return;
    const myGen = ++gen;
    lastFetchAt = Date.now();
    try {
      const res = await fetchImpl(`/api/app?target=${encodeURIComponent(targetId || '')}`);
      if (myGen !== gen) return; // superseded by a later refresh()
      if (res.status === 501) {
        unavailable = true;
        if (deps.onUnavailable) deps.onUnavailable();
        return;
      }
      if (!res.ok) throw new Error(await errorMessage(res));
      const body = await res.json();
      if (myGen !== gen) return; // superseded while awaiting the body
      render(body);
      if (deps.onAvailable) deps.onAvailable();
    } catch (err) {
      if (myGen !== gen) return;
      const message = String((err && err.message) || err);
      if (deps.report) deps.report({ event: 'app-error', err: message });
    }
  }

  // notifyActivity() is what app.js calls on known device activity (a
  // completed touch gesture, a hardware key press -- either can bring a
  // different app to the foreground), rate-limited via shouldRefetch so a
  // drag or a burst of taps does not turn into a burst of /api/app requests.
  function notifyActivity() {
    if (!shouldRefetch(lastFetchAt, Date.now())) return;
    refresh();
  }

  return { refresh, notifyActivity };
}
