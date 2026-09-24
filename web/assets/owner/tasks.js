// ════════════════════════════════════════════════════════════
// tasks.js — Entry point bindings (v2)
// All form logic lives in openTaskModal() in render-tasks.js
// ════════════════════════════════════════════════════════════

function bindTaskHandlers() {
    const btn = qs('#newTaskBtn');
    if (btn) btn.addEventListener('click', () => openTaskModal('create'));

    // ESC closes modal
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') closeModal();
    });
}
