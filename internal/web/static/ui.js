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

// Collapsible sidebar (desktop): icon-only rail, preference remembered. When collapsed, the logo
// becomes an "expand" control (shown on hover/focus), like Notion or ChatGPT.
function sidebarA11y() {
  var c = document.documentElement.classList.contains('sb-collapsed');
  var t = document.querySelector('[data-sidebar-toggle]');
  if (t) {
    t.setAttribute('aria-expanded', c ? 'false' : 'true');
    t.setAttribute('aria-label', c ? 'Expand sidebar' : 'Collapse sidebar');
    t.setAttribute('title', c ? 'Expand sidebar' : 'Collapse sidebar');
  }
  var b = document.querySelector('[data-brand]');
  if (b) {
    if (c) { b.setAttribute('role', 'button'); b.setAttribute('aria-label', 'Expand sidebar'); b.setAttribute('title', 'Expand sidebar'); }
    else { b.removeAttribute('role'); b.removeAttribute('aria-label'); b.setAttribute('title', 'Last Seen'); }
  }
}
function toggleSidebar() {
  var c = document.documentElement.classList.toggle('sb-collapsed');
  try { localStorage.setItem('sidebar', c ? 'collapsed' : 'open'); } catch (err) {}
  sidebarA11y();
}
document.addEventListener('click', function (e) {
  var t = e.target && e.target.closest;
  if (!t) return;
  if (e.target.closest('[data-sidebar-toggle]')) { toggleSidebar(); return; }
  if (e.target.closest('[data-brand]') && document.documentElement.classList.contains('sb-collapsed')) { e.preventDefault(); toggleSidebar(); }
});
document.addEventListener('DOMContentLoaded', sidebarA11y);

// 90-day uptime strip tooltip for visitor page
(function () {
  function initTooltip() {
    var tip = document.getElementById('strip-tooltip');
    if (!tip) return;

    var dateEl = tip.querySelector('[data-tt-date]');
    var dotEl = tip.querySelector('[data-tt-dot]');
    var pctEl = tip.querySelector('[data-tt-pct]');
    var detailEl = tip.querySelector('[data-tt-detail]');
    var arrowEl = tip.querySelector('[data-tt-arrow]');

    var activeBar = null;

    function updateTooltip(bar) {
      if (!bar) return;
      var date = bar.getAttribute('data-date') || '';
      var pct = bar.getAttribute('data-pct') || '';
      var status = bar.getAttribute('data-status') || 'none';
      var detail = bar.getAttribute('data-detail') || '';

      if (dateEl) dateEl.textContent = date;
      if (pctEl) pctEl.textContent = pct;
      if (detailEl) detailEl.textContent = detail ? '· ' + detail : '';

      if (dotEl) {
        dotEl.className = 'inline-block h-2 w-2 rounded-full shrink-0 dot-' + status;
      }

      positionTooltip(bar);
    }

    function positionTooltip(bar) {
      var box = bar.getBoundingClientRect();
      var barCenterX = box.left + box.width / 2;
      var barTop = box.top;
      var barBottom = box.bottom;

      tip.classList.add('is-visible');
      var tipW = tip.offsetWidth;
      var tipH = tip.offsetHeight;

      var pad = 12;
      var x = barCenterX;
      if (x - tipW / 2 < pad) {
        x = pad + tipW / 2;
      } else if (x + tipW / 2 > window.innerWidth - pad) {
        x = window.innerWidth - pad - tipW / 2;
      }

      if (arrowEl) {
        var arrowOffset = barCenterX - x;
        var maxOffset = (tipW / 2) - 12;
        if (arrowOffset < -maxOffset) arrowOffset = -maxOffset;
        if (arrowOffset > maxOffset) arrowOffset = maxOffset;
        arrowEl.style.left = 'calc(50% + ' + arrowOffset + 'px)';
      }

      var y = barTop - tipH - 8;
      if (y < pad) {
        y = barBottom + 8;
        tip.classList.add('arrow-top');
      } else {
        tip.classList.remove('arrow-top');
      }

      tip.style.left = x + 'px';
      tip.style.top = y + 'px';
    }

    function hideTooltip() {
      activeBar = null;
      tip.classList.remove('is-visible');
    }

    document.addEventListener('pointerover', function (e) {
      var bar = e.target && e.target.closest && e.target.closest('.strip-bar');
      if (!bar) return;
      activeBar = bar;
      updateTooltip(bar);
    });

    document.addEventListener('pointerout', function (e) {
      var bar = e.target && e.target.closest && e.target.closest('.strip-bar');
      if (!bar) return;
      var next = e.relatedTarget && e.relatedTarget.closest && e.relatedTarget.closest('.strip-bar');
      if (next) return;
      hideTooltip();
    });

    document.addEventListener('focusin', function (e) {
      var bar = e.target && e.target.closest && e.target.closest('.strip-bar');
      if (bar) {
        activeBar = bar;
        updateTooltip(bar);
      }
    });

    document.addEventListener('focusout', function (e) {
      var bar = e.target && e.target.closest && e.target.closest('.strip-bar');
      if (bar) hideTooltip();
    });

    window.addEventListener('scroll', function () {
      if (activeBar) positionTooltip(activeBar);
    }, { passive: true });

    window.addEventListener('resize', function () {
      if (activeBar) positionTooltip(activeBar);
    }, { passive: true });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', initTooltip);
  } else {
    initTooltip();
  }
})();


// Live filter: <input data-filter="#id"> hides rows (tr[data-row]) inside #id that do not contain the text.
document.addEventListener('input', function (e) {
  var el = e.target;
  var sel = el && el.getAttribute && el.getAttribute('data-filter');
  if (!sel) return;
  var q = el.value.trim().toLowerCase();
  document.querySelectorAll(sel + ' tr[data-row]').forEach(function (r) {
    r.hidden = !!q && r.getAttribute('data-row').toLowerCase().indexOf(q) === -1;
  });
});
