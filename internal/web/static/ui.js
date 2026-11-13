// Confirm dialogs for forms marked data-confirm (kept external so the CSP can forbid inline scripts).
document.addEventListener('submit', function (e) {
  var msg = e.target && e.target.getAttribute && e.target.getAttribute('data-confirm');
  if (msg && !window.confirm(msg)) e.preventDefault();
});

// Theme toggle on the public page.
document.addEventListener('click', function (e) {
  var t = e.target && e.target.closest && e.target.closest('[data-theme-toggle]');
  if (!t) return;
  var dark = document.documentElement.classList.toggle('dark');
  try { localStorage.setItem('theme', dark ? 'dark' : 'light'); } catch (err) {}
  t.setAttribute('aria-pressed', dark ? 'true' : 'false');
});


// Modal dialogs (Add/Edit monitor): htmx fills #modal-root; this handles closing, focus and the 422 re-render.
(function () {
  var opener = null;
  function root() { return document.getElementById('modal-root'); }
  function isOpen() { var m = root(); return !!(m && m.firstElementChild); }
  function close() {
    var m = root(); if (m) m.innerHTML = '';
    document.body.classList.remove('modal-open');
    if (opener && opener.focus) opener.focus();
    opener = null;
  }
  document.addEventListener('htmx:beforeRequest', function (e) {
    var el = e.detail.elt;
    if (el && el.hasAttribute && el.hasAttribute('data-modal-open')) opener = el;
  });
  // Validation errors come back as 422 with the form re-rendered; show them instead of discarding.
  document.addEventListener('htmx:beforeSwap', function (e) {
    var x = e.detail.xhr;
    if (x && x.status === 422 && e.detail.target === root()) { e.detail.shouldSwap = true; e.detail.isError = false; }
  });
  document.addEventListener('htmx:afterSwap', function (e) {
    var m = root();
    if (!m || e.detail.target !== m || !m.firstElementChild) return;
    document.body.classList.add('modal-open');
    var f = m.querySelector('.err');
    f = f ? f.parentElement.querySelector('input,select') : m.querySelector('input:not([type=hidden]),select,textarea');
    if (f) f.focus();
  });
  document.addEventListener('click', function (e) {
    var t = e.target;
    if (!t || !t.closest) return;
    if (t.closest('[data-modal-close]') || (t.hasAttribute && t.hasAttribute('data-modal-backdrop'))) close();
  });
  document.addEventListener('keydown', function (e) {
    if (!isOpen()) return;
    if (e.key === 'Escape') { e.preventDefault(); close(); return; }
    if (e.key !== 'Tab') return;
    var items = root().querySelectorAll('a[href],button:not([disabled]),input:not([type=hidden]),select,textarea');
    if (!items.length) return;
    var first = items[0], last = items[items.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  });
})();
