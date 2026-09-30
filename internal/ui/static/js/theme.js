(function () {
  'use strict';

  const storageKey = 'nfs-gate-theme';
  const icons = { light: 'ti-sun', dark: 'ti-moon', auto: 'ti-device-desktop' };
  const media = window.matchMedia('(prefers-color-scheme: dark)');

  function selectedTheme() {
    let value;
    try { value = localStorage.getItem(storageKey); } catch (_) { return 'auto'; }
    return Object.prototype.hasOwnProperty.call(icons, value) ? value : 'auto';
  }

  function applyTheme(theme) {
    const dark = theme === 'dark' || (theme === 'auto' && media.matches);
    document.documentElement.setAttribute('data-bs-theme', dark ? 'dark' : 'light');
    const icon = document.getElementById('theme-icon');
    if (icon) icon.className = 'ti ' + icons[theme];
    document.querySelectorAll('#theme-menu [data-theme]').forEach(function (item) {
      const active = item.getAttribute('data-theme') === theme;
      item.classList.toggle('active', active);
      item.setAttribute('aria-checked', active ? 'true' : 'false');
    });
  }

  // Run before CSS loads to avoid flashing the wrong theme.
  applyTheme(selectedTheme());

  document.addEventListener('DOMContentLoaded', function () {
    const toggle = document.getElementById('theme-toggle');
    const menu = document.getElementById('theme-menu');
    if (!toggle || !menu) return;

    function setOpen(open) {
      menu.classList.toggle('show', open);
      toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    }

    toggle.addEventListener('click', function () { setOpen(!menu.classList.contains('show')); });
    menu.querySelectorAll('[data-theme]').forEach(function (item) {
      item.addEventListener('click', function () {
        const theme = item.getAttribute('data-theme');
        try { localStorage.setItem(storageKey, theme); } catch (_) { /* keep this tab's choice */ }
        applyTheme(theme);
        setOpen(false);
      });
    });
    document.addEventListener('click', function (event) {
      if (!toggle.contains(event.target) && !menu.contains(event.target)) setOpen(false);
    });
    document.addEventListener('keydown', function (event) {
      if (event.key === 'Escape') setOpen(false);
    });
    applyTheme(selectedTheme());
  });

  media.addEventListener('change', function () {
    if (selectedTheme() === 'auto') applyTheme('auto');
  });
})();
