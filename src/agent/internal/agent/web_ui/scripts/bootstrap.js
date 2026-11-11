// Initialize only after every feature script has been evaluated.
loadHistory();
// Incremental refresh is a fallback for SSE; skip it while the tab is hidden.
setInterval(function() {
    if (!document.hidden) loadHistory();
}, 2000);
conversationEl.addEventListener('scroll', loadOlderHistoryNearTop, {passive: true});
// A floating thumbnail preview would detach from its row while scrolling.
conversationEl.addEventListener('scroll', hideThumbPreview, {passive: true});
refreshCurrentLiveActivity();
setInterval(refreshCurrentLiveActivity, 2000);
loadToolCatalog();
autoResizeInput();
connectSSE();
startResourcePolling();
startRealtimeVoiceStatusPolling();

inputEl.addEventListener('input', autoResizeInput);
inputEl.addEventListener('input', renderComposerPrimary);
document.getElementById('fileInput').addEventListener('change', handleFileSelection);
inputEl.addEventListener('paste', handleComposerPaste);
stateModalCloseEl.addEventListener('click', closeStateDetails);
stateModalEl.addEventListener('click', function(event) {
    if (event.target === stateModalEl) closeStateDetails();
});
toolSelectEl.addEventListener('change', syncSelectedTool);
inputEl.addEventListener('keydown', function(event) {
    if (event.key === 'Enter' && !event.shiftKey && !event.isComposing) {
        event.preventDefault();
        sendMessage({fromKeyboard: true});
    }
});

document.addEventListener('click', function(event) {
    const menu = document.getElementById('clearMenu');
    if (menu && menu.open && !menu.contains(event.target)) closeClearMenu();
});

document.addEventListener('keydown', function(event) {
    if (event.key === 'Escape') {
        closeClearMenu();
    }
    if (event.key === 'Escape' && activeStateMessageKey) {
        closeStateDetails();
    }
});
