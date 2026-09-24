// SameLabs console: the only script. Asks before destructive form
// submits (forms carry data-confirm) and prevents double submits.
// Every page works without it.
'use strict';
document.addEventListener('submit', (event) => {
    const form = event.target;
    if (!(form instanceof HTMLFormElement)) return;
    const question = form.dataset.confirm;
    if (question && !window.confirm(question)) {
        event.preventDefault();
        return;
    }
    if (form.dataset.sent) {
        event.preventDefault();
        return;
    }
    if ((form.method || '').toLowerCase() === 'post') form.dataset.sent = '1';
});
// Returning via back/forward cache must not leave forms locked.
window.addEventListener('pageshow', () => {
    document.querySelectorAll('form[data-sent]').forEach((f) => { delete f.dataset.sent; });
});
