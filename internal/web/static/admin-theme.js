// Admin-only dark mode. Uses its own storage key and class so the public page's theme is never touched.
// Loaded in <head> (external file, CSP forbids inline) so the saved choice applies before first paint.
(function () {
  var root = document.documentElement;
  try { if (localStorage.getItem('admin-theme') === 'dark') root.classList.add('adark'); } catch (e) {}
  document.addEventListener('click', function (e) {
    var t = e.target && e.target.closest && e.target.closest('[data-admin-theme-toggle]');
    if (!t) return;
    var dark = root.classList.toggle('adark');
    try { localStorage.setItem('admin-theme', dark ? 'dark' : 'light'); } catch (err) {}
    t.setAttribute('aria-pressed', dark ? 'true' : 'false');
  });
})();
