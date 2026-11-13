// Restores the collapsed-sidebar preference before first paint (external file so the CSP can forbid inline scripts).
(function () {
  try { if (localStorage.getItem('sidebar') === 'collapsed') document.documentElement.classList.add('sb-collapsed'); } catch (e) {}
})();
