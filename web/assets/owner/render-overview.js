// render-overview.js — the key-box renderer (the /owner/key section).
// The old stats-grid overview was replaced by the Turn view (WO-30
// §3, turn.js); what remains here is renderKey.

// renderKey shows MASKED metadata. The one-time raw disclosure
// (state.newKeyOnce, from registration/reset response only) is never
// rendered here — after navigation only masked metadata remains.
function renderKey() {
    const keyBox = qs('#keyBox');
    if (keyBox) {
        keyBox.textContent = state.keyMasked || t('js.owner_key_hidden');
        keyBox.classList.toggle('is-empty', !state.keyMasked);
    }
    const newKeyBox = qs('#newKeyBox');
    if (newKeyBox) {
        newKeyBox.textContent = state.newKeyOnce || '';
        newKeyBox.classList.toggle('is-empty', !state.newKeyOnce);
        const copyBtn = qs('#copyNewKeyBtn');
        if (copyBtn) copyBtn.style.display = state.newKeyOnce ? '' : 'none';
    }
}
