// Applies the saved theme before first paint (external file so the CSP can forbid inline scripts).
(function () {
  try { if (localStorage.getItem('theme') === 'dark') document.documentElement.classList.add('dark'); } catch (e) {}
})();
