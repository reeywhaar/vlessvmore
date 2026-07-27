// The page ships every language and every device already rendered, with all but one of
// each hidden. This only moves the hidden attribute around, so the page is correct before
// any of it runs — and stays correct if none of it does.
(function () {
  'use strict';

  var KINDS = ['lang', 'device'];

  function camel(kind) {
    return 'set' + kind.charAt(0).toUpperCase() + kind.slice(1);
  }

  function apply(kind, value) {
    var matched = false;
    document.querySelectorAll('[data-' + kind + ']').forEach(function (el) {
      var mine = el.dataset[kind] === value;
      el.hidden = !mine;
      matched = matched || mine;
    });
    if (!matched) {
      return false;
    }
    document.querySelectorAll('[data-set-' + kind + ']').forEach(function (el) {
      var mine = el.dataset[camel(kind)] === value;
      el.setAttribute('aria-pressed', mine ? 'true' : 'false');
      if (kind === 'lang' && mine && el.dataset.title) {
        document.title = el.dataset.title;
      }
    });
    if (kind === 'lang') {
      document.documentElement.lang = value;
    }
    try {
      localStorage.setItem('vv.' + kind, value);
    } catch (e) {
      // Private browsing. The choice just will not survive a reload.
    }
    return true;
  }

  KINDS.forEach(function (kind) {
    document.addEventListener('click', function (ev) {
      var btn = ev.target.closest('[data-set-' + kind + ']');
      if (btn) {
        apply(kind, btn.dataset[camel(kind)]);
      }
    });

    // A URL that named this one beats everything: whoever sent the link knew something
    // the browser does not. Applying it also stores it, so it survives a reload.
    var forced = document.body.dataset['forced' + kind.charAt(0).toUpperCase() + kind.slice(1)];
    if (forced) {
      apply(kind, forced);
      return;
    }

    // Otherwise a remembered choice beats the server's guess, which came from headers.
    var saved = null;
    try {
      saved = localStorage.getItem('vv.' + kind);
    } catch (e) {
      saved = null;
    }
    if (saved) {
      apply(kind, saved);
    }
  });

  document.addEventListener('click', function (ev) {
    var btn = ev.target.closest('[data-copy]');
    if (!btn) {
      return;
    }
    var done = function () {
      var original = btn.textContent;
      btn.textContent = btn.dataset.copied || original;
      btn.classList.add('copied');
      setTimeout(function () {
        btn.textContent = original;
        btn.classList.remove('copied');
      }, 1600);
    };

    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(btn.dataset.copy).then(done, select);
    } else {
      select();
    }

    // Falls back to selecting the text so the user can copy it by hand: navigator
    // .clipboard is missing on older WebViews and refused outside a secure context.
    function select() {
      var code = btn.parentNode.querySelector('code');
      if (!code) {
        return;
      }
      var range = document.createRange();
      range.selectNodeContents(code);
      var sel = window.getSelection();
      sel.removeAllRanges();
      sel.addRange(range);
    }
  });
})();
