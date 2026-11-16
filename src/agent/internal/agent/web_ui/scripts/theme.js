// Colour theme: "dark" or "light". An explicit choice is remembered in
// localStorage; without one the page follows the system preference.
// Load this synchronously in <head> so the first paint uses the right theme.
(function() {
    const STORAGE_KEY = 'aico.webui.theme';
    const root = document.documentElement;
    const systemLight = window.matchMedia ? window.matchMedia('(prefers-color-scheme: light)') : null;

    function storedTheme() {
        try {
            const value = localStorage.getItem(STORAGE_KEY);
            return value === 'light' || value === 'dark' ? value : '';
        } catch (_) {
            return '';
        }
    }

    function systemTheme() {
        return systemLight && systemLight.matches ? 'light' : 'dark';
    }

    function apply(theme) {
        root.dataset.theme = theme;
        const toggle = document.getElementById('themeToggle');
        if (toggle) {
            const next = theme === 'light' ? 'dark' : 'light';
            const label = 'Switch to ' + next + ' theme';
            toggle.title = label;
            toggle.setAttribute('aria-label', label);
        }
    }

    apply(storedTheme() || systemTheme());

    if (systemLight) {
        const follow = function() {
            if (!storedTheme()) apply(systemTheme());
        };
        if (systemLight.addEventListener) systemLight.addEventListener('change', follow);
        else if (systemLight.addListener) systemLight.addListener(follow);
    }

    // Keep other open tabs in step with a choice made here.
    window.addEventListener('storage', function(event) {
        if (event.key === STORAGE_KEY) apply(storedTheme() || systemTheme());
    });

    window.toggleTheme = function() {
        const next = root.dataset.theme === 'light' ? 'dark' : 'light';
        try {
            localStorage.setItem(STORAGE_KEY, next);
        } catch (_) {
            // Private mode: the switch still applies to this page.
        }
        apply(next);
    };

    document.addEventListener('DOMContentLoaded', function() {
        apply(root.dataset.theme);
    });
})();
