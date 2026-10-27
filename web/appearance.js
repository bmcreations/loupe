// Appearance: Light, Dark, or System (follow prefers-color-scheme). Persisted
// like handedness, so a viewer who forced Dark gets Dark next session.
//
// System is the absence of a choice: nothing stored, and no data-theme on
// the root, so style.css's prefers-color-scheme block decides. Removing the
// key keeps a viewer who never touched the setting identical to one who set
// it back.
//
// Every storage call is wrapped for the same reason handedness.js wraps
// them: Safari in some private modes throws. index.html's inline script
// repeats readAppearance and applyAppearance by hand so the first paint is
// already themed; keep the key and the two values in step with it.

export const KEY = 'loupe.appearance';

// The background/primary values, for <meta name="theme-color">. Kept here
// rather than read from computed style, because a meta change has to name a
// colour and the computed value is not settled until a style pass.
const BACKGROUND = { light: '#f2f3f5', dark: '#0b0d10' };

export function readAppearance(storage) {
  try {
    const v = storage.getItem(KEY);
    return v === 'light' || v === 'dark' ? v : 'system';
  } catch {
    return 'system';
  }
}

export function writeAppearance(storage, value) {
  try {
    if (value === 'light' || value === 'dark') storage.setItem(KEY, value);
    else storage.removeItem(KEY);
  } catch {
    // ignored: the choice still applies for this page, it just does not stick
  }
}

export function applyAppearance(root, value) {
  if (value === 'light' || value === 'dark') root.dataset.theme = value;
  else delete root.dataset.theme;
}

// themeName resolves System to the theme the page is showing.
export function themeName(value, prefersDark) {
  if (value === 'light' || value === 'dark') return value;
  return prefersDark ? 'dark' : 'light';
}

export function themeColor(value, prefersDark) {
  return BACKGROUND[themeName(value, prefersDark)];
}
