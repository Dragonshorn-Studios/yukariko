/* Yukariko dashboard live patch (see docs/ASSETS.md).
   Original, no frameworks. Progressive enhancement: with JS, swap #main
   and footer from the same GET HTML the no-JS meta refresh would load.
   GET-only, same-origin, never mutates. */
(function () {
  "use strict";
  var meta = document.querySelector('meta[http-equiv="refresh"]');
  var seconds = 12;
  if (meta) {
    var n = parseInt(meta.getAttribute("content"), 10);
    if (n > 0) {
      seconds = n;
    }
    meta.parentNode.removeChild(meta);
  }

  var timer = null;

  function delay() {
    return seconds * 1000;
  }

  function path() {
    return window.location.pathname + window.location.search;
  }

  function replace(sel, doc) {
    var curr = document.querySelector(sel);
    var next = doc.querySelector(sel);
    if (!curr || !next) {
      return;
    }
    if (curr.innerHTML === next.innerHTML) {
      return;
    }
    curr.innerHTML = next.innerHTML;
  }

  function apply(html) {
    var doc = new DOMParser().parseFromString(html, "text/html");
    var nextMeta = doc.querySelector('meta[http-equiv="refresh"]');
    if (nextMeta) {
      var n = parseInt(nextMeta.getAttribute("content"), 10);
      if (n > 0) {
        seconds = n;
      }
    }
    replace("#main", doc);
    replace("footer", doc);
  }

  function arm() {
    if (document.hidden) {
      return;
    }
    if (timer) {
      clearTimeout(timer);
    }
    timer = setTimeout(tick, delay());
  }

  function tick() {
    if (document.hidden) {
      return;
    }
    fetch(path(), { headers: { Accept: "text/html" }, credentials: "same-origin" })
      .then(function (res) {
        if (!res.ok) {
          throw new Error("live refresh failed");
        }
        return res.text();
      })
      .then(apply)
      .catch(function () {})
      .then(arm);
  }

  document.addEventListener("visibilitychange", function () {
    if (document.hidden) {
      if (timer) {
        clearTimeout(timer);
      }
      timer = null;
      return;
    }
    tick();
  });

  arm();
})();
