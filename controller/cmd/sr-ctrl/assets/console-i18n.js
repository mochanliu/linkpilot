/*
 * console-i18n.js -- Chinese/English switch for the LinkPilot console.
 *
 * Why a runtime pass instead of data-i18n keys on every node:
 *   The console renders whole views by building HTML strings and assigning
 *   innerHTML (about 1,800 distinct Chinese sentences across 40 views). Keying
 *   every string would mean touching every render function and keeping two
 *   copies of the markup in sync forever. Instead the Chinese text stays the
 *   single source of truth in console.html, and this file maps it to English at
 *   the moment it is inserted. A view added tomorrow shows up in English for
 *   free once its strings are in the dictionary; until then it falls back to
 *   Chinese rather than showing blanks.
 *
 * Load order: i18n-en.js defines SR_I18N_DICT / SR_I18N_RULES, then this file
 * installs the engine. Both are served from /assets/ and change with the binary.
 *
 * Nothing here translates: <script>, <style>, <pre>, <code>, <textarea>, input
 * values, or anything inside an element with data-i18n-skip. Placeholders, title,
 * aria-label and alt are translated.
 */
(function () {
  "use strict";

  var DICT = window.SR_I18N_DICT || {};
  // Drop malformed entries instead of trusting the file: a hand-edited rule list
  // can grow a hole (a missing comma makes the array swallow nothing and leave an
  // undefined slot), and one bad entry would otherwise throw on every unmatched
  // string and leave whole views untranslated.
  var RULES = (window.SR_I18N_RULES || []).filter(function (r) {
    return r && r[0] instanceof RegExp && typeof r[1] === "string";
  });
  var CJK = /[\u3400-\u9fff\uf900-\ufaff]/;
  var ATTRS = ["title", "placeholder", "aria-label", "alt"];
  var SKIP = {SCRIPT: 1, STYLE: 1, PRE: 1, CODE: 1, TEXTAREA: 1, KBD: 1, SAMP: 1};

  // node -> the Chinese text we replaced, so switching back is lossless.
  var textOrig = new WeakMap();
  // element -> {attr: original value}
  var attrOrig = new WeakMap();

  var lang = "zh";
  var observer = null;
  var queued = null;
  var pending = [];       // nodes added while a render is in flight
  var missing = Object.create(null);  // zh -> count, only in debug mode
  var debug = false;

  function lookup(s) {
    var key = s.trim();
    if (!key) return null;
    if (Object.prototype.hasOwnProperty.call(DICT, key)) return DICT[key];
    return null;
  }

  // Rules run against the whole (trimmed) string, in order, first hit wins.
  // They exist for sentences assembled at runtime, e.g. "共 3 台设备".
  function applyRules(s) {
    for (var i = 0; i < RULES.length; i++) {
      var re = RULES[i][0];
      try {
        if (re.test(s)) return s.replace(re, RULES[i][1]);
      } catch (e) {
        // A rule with a bad replacement must not take the whole pass down.
        continue;
      }
    }
    return null;
  }

  function translate(s) {
    var key = s.trim();
    if (!key || !CJK.test(key)) return s;
    var hit = lookup(key);
    if (hit === null) hit = applyRules(key);
    if (hit === null) {
      if (debug) missing[key] = (missing[key] || 0) + 1;
      return s;
    }
    // Keep the original surrounding whitespace: text nodes carry inline spacing.
    var lead = s.match(/^\s*/)[0];
    var trail = s.match(/\s*$/)[0];
    return lead + hit + trail;
  }

  function skipped(el) {
    if (!el) return true;
    if (SKIP[el.nodeName]) return true;
    if (el.getAttribute && el.getAttribute("data-i18n-skip") !== null) return true;
    return false;
  }

  function attrText(el, name) {
    var v = el.getAttribute(name);
    if (!v || !CJK.test(v)) return null;
    var out = translate(v);
    return out === v ? null : out;
  }

  function walk(root) {
    if (lang !== "en" || !root) return;
    if (root.nodeType === 3) {
      var parent = root.parentNode;
      if (!parent || skipped(parent) || textOrig.has(root)) return;
      var t = translate(root.nodeValue);
      if (t !== root.nodeValue) {
        textOrig.set(root, root.nodeValue);
        root.nodeValue = t;
      }
      return;
    }
    if (root.nodeType !== 1) return;
    if (skipped(root) && root.nodeName !== "BODY") return;
    for (var i = 0; i < ATTRS.length; i++) {
      var name = ATTRS[i];
      if (!root.hasAttribute || !root.hasAttribute(name)) continue;
      var orig = attrText(root, name);
      if (orig === null) continue;
      var bag = attrOrig.get(root);
      if (!bag) { bag = {}; attrOrig.set(root, bag); }
      if (!(name in bag)) bag[name] = root.getAttribute(name);
      root.setAttribute(name, orig);
    }
    for (var c = root.firstChild; c; c = c.nextSibling) walk(c);
  }

  function restore(root) {
    if (!root) return;
    var stack = [root];
    while (stack.length) {
      var n = stack.pop();
      if (n.nodeType === 3) {
        if (textOrig.has(n)) { n.nodeValue = textOrig.get(n); textOrig.delete(n); }
        continue;
      }
      if (n.nodeType !== 1) continue;
      var bag = attrOrig.get(n);
      if (bag) {
        for (var k in bag) if (Object.prototype.hasOwnProperty.call(bag, k)) n.setAttribute(k, bag[k]);
        attrOrig.delete(n);
      }
      for (var c = n.firstChild; c; c = c.nextSibling) stack.push(c);
    }
  }

  function flush() {
    queued = null;
    if (lang !== "en") return;
    var batch = pending;
    pending = [];
    for (var i = 0; i < batch.length; i++) walk(batch[i]);
  }

  function schedule(node) {
    pending.push(node);
    if (queued) return;
    // One frame, so a view that assigns innerHTML five times translates once.
    queued = (window.requestAnimationFrame || function (f) { return setTimeout(f, 16); })(flush);
  }

  function observe() {
    if (observer || !window.MutationObserver) return;
    observer = new MutationObserver(function (records) {
      for (var i = 0; i < records.length; i++) {
        var r = records[i];
        if (r.type === "childList") {
          for (var j = 0; j < r.addedNodes.length; j++) {
            var n = r.addedNodes[j];
            if (n.nodeType === 1 || n.nodeType === 3) schedule(n);
          }
        } else if (r.type === "attributes" && r.attributeName) {
          if (ATTRS.indexOf(r.attributeName) >= 0) schedule(r.target);
        }
      }
    });
    observer.observe(document.documentElement, {
      childList: true, subtree: true, attributes: true,
      attributeFilter: ATTRS, characterData: false
    });
  }

  function setLang(next, opts) {
    next = next === "en" ? "en" : "zh";
    opts = opts || {};
    var body = document.body;
    if (next === lang && !opts.force) return lang;
    if (next === "en") {
      lang = "en";
      document.documentElement.setAttribute("lang", "en");
      document.title = translate(document.title);
      if (body) walk(body);
      observe();
    } else {
      lang = "zh";
      document.documentElement.setAttribute("lang", "zh-CN");
      if (body) restore(body);
      // Restored nodes are new originals again; nothing to keep observing for.
      if (observer) { observer.disconnect(); observer = null; }
    }
    try { localStorage.setItem("sr-lang", lang); } catch (e) { /* private mode */ }
    document.dispatchEvent(new CustomEvent("sr-lang", {detail: {lang: lang}}));
    return lang;
  }

  function initial() {
    var fromQuery = null;
    try {
      fromQuery = new URLSearchParams(window.location.search).get("lang");
    } catch (e) { /* older browser */ }
    var stored = null;
    try { stored = localStorage.getItem("sr-lang"); } catch (e) { /* private mode */ }
    if (fromQuery === "en" || fromQuery === "zh") return fromQuery;
    if (stored === "en" || stored === "zh") return stored;
    // First visit: follow the browser. A zh-* browser gets Chinese; anything else
    // gets English, which is the right default for an operator who found this
    // repository by accident and the wrong one only for a Chinese browser that
    // is set to English -- and that one click fixes it for good.
    var nav = (navigator.language || "en").toLowerCase();
    return nav.indexOf("zh") === 0 ? "zh" : "en";
  }

  function init() {
    debug = /[?&]i18n-debug=1/.test(window.location.search);
    var pill = document.getElementById("lang-pill");
    if (pill) {
      pill.addEventListener("click", function (ev) {
        var b = ev.target.closest("[data-lang]");
        if (!b) return;
        setLang(b.getAttribute("data-lang"));
        syncPill();
      });
    }
    setLang(initial());
    syncPill();
    if (debug) {
      window.SR_I18N.report = function () {
        var rows = Object.keys(missing).sort(function (a, b) { return missing[b] - missing[a]; });
        console.table(rows.map(function (k) { return {zh: k, hits: missing[k]}; }));
        return rows;
      };
    }
  }

  function syncPill() {
    var pill = document.getElementById("lang-pill");
    if (!pill) return;
    var btns = pill.querySelectorAll("[data-lang]");
    for (var i = 0; i < btns.length; i++) {
      btns[i].classList.toggle("on", btns[i].getAttribute("data-lang") === lang);
    }
  }

  window.SR_I18N = {
    init: init,
    setLang: setLang,
    lang: function () { return lang; },
    t: translate,
    // For code that builds strings and needs a translation immediately.
    s: function (zh) { return lang === "en" ? translate(zh) : zh; },
    missing: function () { return missing; },
    size: function () { return Object.keys(DICT).length; }
  };
})();
