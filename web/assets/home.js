// home.js — homepage interactions (WO-19 H1/H3). One behaviour: copy a
// task code to the clipboard. The button carries its translated labels
// as data attributes (server-rendered), so this file needs no i18n and
// no inline event handlers anywhere.

document.addEventListener('DOMContentLoaded', () => {
    document.querySelectorAll('[data-copy-code]').forEach((btn) => {
        btn.addEventListener('click', async () => {
            const code = btn.getAttribute('data-copy-code') || '';
            if (!code) return;
            try {
                await navigator.clipboard.writeText(code);
            } catch (e) {
                return;
            }
            const label = btn.getAttribute('data-label') || btn.textContent;
            const done = btn.getAttribute('data-done') || label;
            btn.textContent = done;
            btn.classList.add('is-copied');
            setTimeout(() => {
                btn.textContent = label;
                btn.classList.remove('is-copied');
            }, 1600);
        });
    });
});
