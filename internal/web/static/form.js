// Live "time to confirm an outage" estimate on the monitor form (page and modal).
function initMonitorForm() {
  var r = document.getElementById('max_retries'), d = document.getElementById('retry_delay_s'), e = document.getElementById('estimate');
  if (!r || !d || !e) return;
  function fmt(s) {
    if (s < 60) return s + ' seconds';
    if (s < 3600) { var m = Math.round(s / 6) / 10; return m + ' minute' + (m === 1 ? '' : 's'); }
    var h = Math.round(s / 360) / 10; return h + ' hour' + (h === 1 ? '' : 's');
  }
  function upd() {
    var n = parseInt(r.value, 10), s = parseInt(d.value, 10);
    if (isNaN(n) || isNaN(s)) { e.textContent = ''; return; }
    e.textContent = n === 0
      ? 'An incident opens and you are emailed on the first failed check.'
      : 'After the first failure, an outage is confirmed and you are emailed about ' + fmt(n * s) + ' later if every retry also fails.';
  }
  r.addEventListener('input', upd); d.addEventListener('input', upd); upd();
}
document.addEventListener('DOMContentLoaded', initMonitorForm);
document.addEventListener('htmx:afterSwap', initMonitorForm);
