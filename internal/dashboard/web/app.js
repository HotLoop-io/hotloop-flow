(function () {
  "use strict";

  var base = location.pathname.replace(/[^/]*$/, "");
  var CELL = 48, GAP = 6;
  var $ = function (id) { return document.getElementById(id); };
  var state = new Map();       // widget id -> {msg, value, points}
  var live = new Map();        // widget id -> {set(st, update)}
  var layout = null;
  var activeTab = null;
  var menu = $("menu"), page = $("page"), titleEl = $("title"), conn = $("conn");

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function num(v, d) { var n = parseFloat(v); return isFinite(n) ? n : d; }
  function intv(v, d) { var n = parseInt(v, 10); return isFinite(n) ? n : d; }

  // ------------------------------------------------------------------
  // Expressions: {{ msg.payload | number:1 }} and the Angular-style
  // expressions a ui_template uses. A scope is a Proxy so an assignment
  // lands on the scope and a name that is not there reads as undefined.
  // ------------------------------------------------------------------
  var SAFE = new Set(["Math", "JSON", "Date", "Number", "String", "Boolean", "Array", "Object", "parseInt",
    "parseFloat", "isNaN", "isFinite", "undefined", "NaN", "Infinity", "console", "window", "document", "Symbol", "RegExp"]);
  function makeScope(target) {
    return new Proxy(target, {
      has: function (t, k) { return typeof k === "string" && !SAFE.has(k); },
      get: function (t, k) { if (k === Symbol.unscopables) return undefined; return t[k]; },
    });
  }
  var cache = new Map();
  function compile(expr) {
    var f = cache.get(expr);
    if (!f) {
      try { f = new Function("$s", "with($s){ return (" + expr + "\n); }"); } // eslint-disable-line no-new-func
      catch (e) { f = function () { return undefined; }; }
      cache.set(expr, f);
    }
    return f;
  }
  function evalIn(expr, scope) {
    try { return compile(expr)(scope); } catch (e) { return undefined; }
  }
  function runStatement(expr, scope) {
    try { new Function("$s", "with($s){ " + expr + "\n }")(scope); } catch (e) { /* a bad expression shows nothing */ } // eslint-disable-line no-new-func
  }
  function filter(v, spec) {
    var m = /^\s*(\w+)\s*(?::\s*(.+))?$/.exec(spec);
    if (!m) return v;
    if (m[1] === "number") { var n = Number(v); return isFinite(n) ? n.toFixed(intv(m[2], 0)) : v; }
    if (m[1] === "uppercase") return String(v).toUpperCase();
    if (m[1] === "lowercase") return String(v).toLowerCase();
    return v;
  }
  // interpolate replaces each {{ expr }} in a string.
  function interpolate(str, scope) {
    return String(str == null ? "" : str).replace(/\{\{([\s\S]*?)\}\}/g, function (_, inner) {
      var parts = inner.split(/(?<!\|)\|(?!\|)/);
      var v = evalIn(parts[0], scope);
      for (var i = 1; i < parts.length; i++) v = filter(v, parts[i]);
      return v == null ? "" : typeof v === "object" ? JSON.stringify(v) : String(v);
    });
  }
  // setLabel shows a label. <br> breaks a line; any other markup is text.
  function setLabel(node, html) {
    node.textContent = "";
    String(html == null ? "" : html).split(/<br\s*\/?>/i).forEach(function (line, i) {
      if (i) node.appendChild(document.createElement("br"));
      node.appendChild(document.createTextNode(line.replace(/<[^>]*>/g, "")));
    });
  }

  function emit(id, event) {
    return fetch(base + "api/emit", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: id, event: event }),
    }).catch(function () { conn.dataset.state = "lost"; });
  }

  function valueOf(st) {
    if (!st) return undefined;
    if (st.value !== undefined && st.value !== null) return st.value;
    return st.msg ? st.msg.payload : undefined;
  }

  // ------------------------------------------------------------------
  // Widgets. Each make function returns {el, set(st)}; set is called with
  // the widget's state when it arrives or changes.
  // ------------------------------------------------------------------
  var makers = {};

  makers.ui_text = function (d) {
    var p = d.props, root = el("div", "w w-text " + (p.layout || "row-spread"));
    var lbl = el("span", "lbl"), val = el("span", "val");
    setLabel(lbl, p.label);
    if (p.layout && p.layout.indexOf("col") === 0) { /* label above the value */ }
    root.append(lbl, val);
    if (p.style) {
      if (p.fontSize) val.style.fontSize = p.fontSize + "px";
      if (p.color) val.style.color = p.color;
      if (p.font) val.style.fontFamily = p.font;
    }
    return { el: root, set: function (st) {
      var msg = st && st.msg || {};
      val.textContent = interpolate(p.format || "{{msg.payload}}", makeScope({ msg: msg, value: msg.payload }));
      if (p.label && /\{\{/.test(p.label)) setLabel(lbl, interpolate(p.label, makeScope({ msg: msg })));
    } };
  };

  makers.ui_numeric = function (d) {
    var p = d.props, root = el("div", "w w-numeric");
    var lbl = el("span", "lbl"), input = el("input"), unit = el("span", "val");
    var dec = el("button", "step", "−"), inc = el("button", "step", "+");
    dec.type = inc.type = "button";
    dec.setAttribute("aria-label", "Decrease"); inc.setAttribute("aria-label", "Increase");
    setLabel(lbl, p.label);
    input.type = "number";
    var min = p.min === "" || p.min == null ? null : num(p.min, null), max = p.max === "" || p.max == null ? null : num(p.max, null);
    var step = num(p.step, 1);
    if (min != null) input.min = min;
    if (max != null) input.max = max;
    input.step = step;
    input.setAttribute("aria-label", String(p.label || p.name || "value").replace(/<[^>]*>/g, " "));
    root.append(lbl, dec, input, inc, unit);
    var current = null;
    function send(v) {
      if (!isFinite(v)) return;
      if (min != null && v < min) v = min;
      if (max != null && v > max) v = max;
      current = v; input.value = v;
      emit(d.id, { event: "change", value: v });
    }
    input.addEventListener("change", function () { if (input.value !== "") send(Number(input.value)); });
    dec.addEventListener("click", function () { send((current != null ? current : num(input.value, 0)) - step); });
    inc.addEventListener("click", function () { send((current != null ? current : num(input.value, 0)) + step); });
    return { el: root, set: function (st) {
      var v = valueOf(st);
      if (v === undefined || v === null || v === "") return;
      current = Number(v);
      if (document.activeElement !== input) input.value = isFinite(current) ? current : "";
      var f = String(p.format || "{{value}}");
      unit.textContent = interpolate(f, makeScope({ value: v, msg: st.msg || {} })).replace(String(v), "").trim();
    } };
  };

  makers.ui_button = function (d) {
    var p = d.props, root = el("div", "w w-button");
    var b = el("button", "btn"); b.type = "button";
    setLabel(b, p.label);
    if (p.bgcolor) b.style.background = p.bgcolor;
    if (p.color) b.style.color = p.color;
    if (p.tooltip) b.title = p.tooltip;
    b.addEventListener("click", function () { emit(d.id, { event: "click" }); });
    root.appendChild(b);
    return { el: root, set: function (st) {
      var msg = st && st.msg || {};
      if (msg.label !== undefined && p.label && /\{\{/.test(p.label)) setLabel(b, interpolate(p.label, makeScope({ msg: msg })));
    } };
  };

  makers.ui_switch = function (d) {
    var p = d.props, root = el("div", "w w-switch");
    var lbl = el("span", "lbl"), sw = el("button", "sw");
    sw.type = "button"; sw.setAttribute("role", "switch"); sw.setAttribute("aria-checked", "false");
    setLabel(lbl, p.label);
    sw.setAttribute("aria-label", String(p.label || p.name || "switch").replace(/<[^>]*>/g, " "));
    if (p.tooltip) sw.title = p.tooltip;
    root.append(lbl, sw);
    var on = false;
    function show(v) {
      on = !!v; sw.setAttribute("aria-checked", on ? "true" : "false");
      var c = on ? p.oncolor : p.offcolor;
      sw.style.background = c || "";
    }
    sw.addEventListener("click", function () {
      var next = !on;
      if (String(p.decouple) !== "true") show(next);
      emit(d.id, { event: "change", value: next });
    });
    return { el: root, set: function (st) { if (st && st.value !== undefined && st.value !== null) show(st.value); } };
  };

  makers.ui_text_input = function (d) {
    var p = d.props, root = el("div", "w w-input");
    var lbl = el("span", "lbl"), input = el("input");
    setLabel(lbl, p.label);
    var mode = p.mode || "text";
    input.type = ["text", "email", "password", "number", "time", "date", "color"].indexOf(mode) >= 0 ? mode : "text";
    input.setAttribute("aria-label", String(p.label || p.name || "text").replace(/<[^>]*>/g, " "));
    if (p.tooltip) input.title = p.tooltip;
    root.append(lbl, input);
    var timer = null;
    function send() {
      var v = input.value;
      emit(d.id, { event: "change", value: input.type === "number" ? (v === "" ? null : Number(v)) : v });
    }
    input.addEventListener("input", function () {
      if (String(p.sendOnBlur) === "true" || p.sendOnBlur === true) return;
      clearTimeout(timer);
      timer = setTimeout(send, Math.max(0, num(p.delay, 300)));
    });
    input.addEventListener("blur", function () { if (String(p.sendOnBlur) === "true" || p.sendOnBlur === true) send(); });
    input.addEventListener("keydown", function (e) { if (e.key === "Enter") { clearTimeout(timer); send(); } });
    return { el: root, set: function (st) {
      var v = valueOf(st);
      if (v !== undefined && v !== null && document.activeElement !== input) input.value = v;
    } };
  };

  makers.ui_led = function (d) {
    var p = d.props, root = el("div", "w w-led" + (p.labelPlacement === "right" ? " right" : ""));
    var lbl = el("span", "lbl"), led = el("span", "led" + (p.shape === "square" ? " square" : p.shape === "rounded" ? " rounded" : "") + (p.showGlow === false ? "" : " glow"));
    lbl.style.textAlign = p.labelAlignment || "left";
    setLabel(lbl, p.label);
    led.setAttribute("role", "img");
    root.append(lbl, led);
    function colorFor(msg) {
      if (String(p.allowColorForValueInMessage) === "true" && msg.color) return msg.color;
      var v = msg.payload;
      var list = Array.isArray(p.colorForValue) ? p.colorForValue : [];
      for (var i = 0; i < list.length; i++) {
        var e = list[i], hit = false;
        if (e.valueType === "num") hit = Number(v) === Number(e.value);
        else if (e.valueType === "bool") hit = String(v) === String(e.value);
        else hit = String(v) === String(e.value);
        if (hit) return e.color;
      }
      return null;
    }
    return { el: root, set: function (st) {
      var msg = st && st.msg || {}, c = colorFor(msg);
      led.style.background = c || "";
      led.style.setProperty("--led", c || "transparent");
      led.setAttribute("aria-label", (String(p.label || "").replace(/<[^>]*>/g, " ") + " " + (msg.payload === undefined ? "no value" : String(msg.payload))).trim());
    } };
  };

  // ---- gauge -------------------------------------------------------
  function sizeCanvas(cv) {
    var r = cv.getBoundingClientRect(), dpr = window.devicePixelRatio || 1;
    var w = Math.max(10, Math.round(r.width * dpr)), h = Math.max(10, Math.round(r.height * dpr));
    if (cv.width !== w || cv.height !== h) { cv.width = w; cv.height = h; }
    return { w: w, h: h, dpr: dpr };
  }
  function cssVar(n) { return getComputedStyle(document.documentElement).getPropertyValue(n).trim(); }

  makers.ui_gauge = function (d) {
    var p = d.props, root = el("div", "w w-gauge");
    var title = el("div", "lbl"), plot = el("div", "plot"), cv = el("canvas"), lbl = el("div", "lbl");
    setLabel(title, p.title);
    plot.appendChild(cv);
    root.append(title, plot, lbl);
    var min = num(p.min, 0), max = num(p.max, 100), cur = null, msgNow = {};
    var seg1 = num(p.seg1, NaN), seg2 = num(p.seg2, NaN);
    var colors = Array.isArray(p.colors) && p.colors.length ? p.colors : ["#00b500", "#e6e600", "#ca3838"];
    function colorAt(v) {
      if (isFinite(seg1) && isFinite(seg2) && String(p.diff) !== "true") {
        return v < seg1 ? colors[0] : v < seg2 ? colors[1] || colors[0] : colors[2] || colors[0];
      }
      return colors[0];
    }
    function draw() {
      var s = sizeCanvas(cv), g = cv.getContext("2d"), v = cur == null ? min : Math.min(max, Math.max(min, cur));
      g.clearRect(0, 0, s.w, s.h);
      if (s.w < 60 || s.h < 40) return; // not laid out yet, or too small to read
      var frac = max === min ? 0 : (v - min) / (max - min), type = p.gtype || "gage";
      var text = cssVar("--text"), line = cssVar("--line");
      if (type === "gage" || type === "donut") {
        var cx = s.w / 2, cy = type === "gage" ? s.h * 0.88 : s.h / 2;
        var r = Math.max(8, Math.min(s.w / 2, type === "gage" ? s.h * 0.85 : s.h / 2) - 6 * s.dpr), lw = Math.max(6, r * 0.22);
        var a0 = type === "gage" ? Math.PI : -Math.PI / 2, span = type === "gage" ? Math.PI : Math.PI * 2;
        g.lineWidth = lw; g.lineCap = "butt";
        g.strokeStyle = line; g.beginPath(); g.arc(cx, cy, r - lw / 2, a0, a0 + span); g.stroke();
        if (type === "gage" && isFinite(seg1) && isFinite(seg2)) {
          var bounds = [min, seg1, seg2, max];
          for (var i = 0; i < 3; i++) {
            var f0 = (bounds[i] - min) / (max - min), f1 = (bounds[i + 1] - min) / (max - min);
            g.strokeStyle = colors[i] || colors[0];
            g.beginPath(); g.arc(cx, cy, r - lw / 2, a0 + span * f0, a0 + span * f1); g.stroke();
          }
          var ang = a0 + span * frac;
          g.strokeStyle = text; g.lineWidth = 3 * s.dpr; g.beginPath();
          g.moveTo(cx, cy); g.lineTo(cx + Math.cos(ang) * (r - lw), cy + Math.sin(ang) * (r - lw)); g.stroke();
          g.fillStyle = text; g.beginPath(); g.arc(cx, cy, 4 * s.dpr, 0, 7); g.fill();
        } else {
          g.strokeStyle = colorAt(v); g.beginPath(); g.arc(cx, cy, r - lw / 2, a0, a0 + span * frac); g.stroke();
        }
        g.fillStyle = text; g.textAlign = "center"; g.textBaseline = "middle";
        g.font = "600 " + Math.round(r * 0.34) + "px sans-serif";
        g.fillText(interpolate(p.format || "{{value}}", makeScope({ value: cur == null ? "" : cur, msg: msgNow })), cx, type === "gage" ? cy - r * 0.35 : cy);
        g.font = Math.round(r * 0.14) + "px sans-serif"; g.fillStyle = cssVar("--muted");
        if (type === "gage") { g.textAlign = "left"; g.fillText(String(min), cx - r, cy + 12 * s.dpr); g.textAlign = "right"; g.fillText(String(max), cx + r, cy + 12 * s.dpr); }
      } else if (type === "compass") {
        var ccx = s.w / 2, ccy = s.h / 2, cr = Math.max(8, Math.min(s.w, s.h) / 2 - 8 * s.dpr);
        g.strokeStyle = line; g.lineWidth = 2 * s.dpr; g.beginPath(); g.arc(ccx, ccy, cr, 0, 7); g.stroke();
        g.fillStyle = text; g.font = Math.round(cr * 0.2) + "px sans-serif"; g.textAlign = "center"; g.textBaseline = "middle";
        [["N", 0], ["E", 90], ["S", 180], ["W", 270]].forEach(function (c) {
          var a = (c[1] - 90) * Math.PI / 180; g.fillText(c[0], ccx + Math.cos(a) * cr * 0.8, ccy + Math.sin(a) * cr * 0.8);
        });
        var da = ((cur == null ? 0 : cur) - 90) * Math.PI / 180;
        g.strokeStyle = colorAt(v); g.lineWidth = 4 * s.dpr; g.beginPath(); g.moveTo(ccx, ccy);
        g.lineTo(ccx + Math.cos(da) * cr * 0.6, ccy + Math.sin(da) * cr * 0.6); g.stroke();
      } else { // wave
        var wx = s.w * 0.2, ww = s.w * 0.6, wy = 6 * s.dpr, wh = s.h - 12 * s.dpr;
        g.strokeStyle = line; g.lineWidth = 2 * s.dpr; g.strokeRect(wx, wy, ww, wh);
        g.fillStyle = colorAt(v); g.globalAlpha = 0.75; g.fillRect(wx, wy + wh * (1 - frac), ww, wh * frac); g.globalAlpha = 1;
        g.fillStyle = text; g.font = "600 " + Math.round(wh * 0.18) + "px sans-serif"; g.textAlign = "center"; g.textBaseline = "middle";
        g.fillText(interpolate(p.format || "{{value}}", makeScope({ value: cur == null ? "" : cur, msg: msgNow })), wx + ww / 2, wy + wh / 2);
      }
    }
    new ResizeObserver(draw).observe(plot);
    return { el: root, set: function (st) {
      msgNow = st && st.msg || {};
      var v = valueOf(st); cur = v === undefined || v === null || v === "" ? null : Number(v);
      if (cur !== null && !isFinite(cur)) cur = null;
      setLabel(lbl, interpolate(p.label || "", makeScope({ msg: msgNow })));
      draw();
    } };
  };

  // ---- chart -------------------------------------------------------
  var PALETTE = ["#1f77b4", "#ff7f0e", "#2ca02c", "#d62728", "#9467bd", "#8c564b", "#e377c2", "#7f7f7f", "#bcbd22"];
  makers.ui_chart = function (d) {
    var p = d.props, root = el("div", "w w-chart");
    var title = el("div", "lbl"), plot = el("div", "plot"), cv = el("canvas"), nodata = el("div", "nodata"), legend = el("div", "legend");
    setLabel(title, p.label);
    nodata.textContent = p.nodata || "No data";
    plot.append(cv, nodata);
    root.append(title, plot);
    if (String(p.legend) === "true") root.append(legend);
    var colors = Array.isArray(p.colors) && p.colors.length ? p.colors : PALETTE;
    var points = [], pending = false;
    var keepMs = num(p.removeOlder, 1) * num(p.removeOlderUnit, 3600) * 1000;
    var type = p.chartType || "line";
    function series() {
      var m = new Map();
      points.forEach(function (pt) { if (!m.has(pt.s)) m.set(pt.s, []); m.get(pt.s).push(pt); });
      return m;
    }
    function fmtTime(t) {
      var dt = new Date(t), f = String(p.xformat || "HH:mm:ss"), utc = String(p.useUTC) === "true";
      var H = utc ? dt.getUTCHours() : dt.getHours(), M = utc ? dt.getUTCMinutes() : dt.getMinutes(), S = utc ? dt.getUTCSeconds() : dt.getSeconds();
      var z = function (n) { return (n < 10 ? "0" : "") + n; };
      return f.replace("HH", z(H)).replace("mm", z(M)).replace("ss", z(S));
    }
    function draw() {
      pending = false;
      var s = sizeCanvas(cv), g = cv.getContext("2d");
      g.clearRect(0, 0, s.w, s.h);
      nodata.style.display = points.length ? "none" : "flex";
      if (!points.length) return;
      var text = cssVar("--muted"), line = cssVar("--line");
      var padL = 44 * s.dpr, padR = 8 * s.dpr, padT = 8 * s.dpr, padB = 20 * s.dpr;
      var t1 = points[points.length - 1].t, t0 = keepMs > 0 ? t1 - keepMs : points[0].t;
      if (t1 === t0) t1 = t0 + 1000;
      var lo = Infinity, hi = -Infinity;
      points.forEach(function (pt) { if (pt.v < lo) lo = pt.v; if (pt.v > hi) hi = pt.v; });
      if (p.ymin !== "" && p.ymin != null && isFinite(Number(p.ymin))) lo = Number(p.ymin);
      if (p.ymax !== "" && p.ymax != null && isFinite(Number(p.ymax))) hi = Number(p.ymax);
      if (lo === hi) { lo -= 1; hi += 1; }
      var X = function (t) { return padL + (t - t0) / (t1 - t0) * (s.w - padL - padR); };
      var Y = function (v) { return padT + (1 - (v - lo) / (hi - lo)) * (s.h - padT - padB); };
      g.font = Math.round(10 * s.dpr) + "px sans-serif"; g.fillStyle = text; g.strokeStyle = line; g.lineWidth = 1;
      g.textAlign = "right"; g.textBaseline = "middle";
      for (var i = 0; i <= 4; i++) {
        var v = lo + (hi - lo) * i / 4, y = Y(v);
        g.beginPath(); g.moveTo(padL, y); g.lineTo(s.w - padR, y); g.stroke();
        g.fillText(Math.abs(v) >= 100 ? v.toFixed(0) : v.toFixed(1), padL - 4 * s.dpr, y);
      }
      g.textAlign = "center"; g.textBaseline = "top";
      for (var j = 0; j <= 3; j++) { var t = t0 + (t1 - t0) * j / 3; g.fillText(fmtTime(t), X(t), s.h - padB + 4 * s.dpr); }
      var names = [];
      series().forEach(function (pts, name) {
        var col = colors[names.length % colors.length]; names.push([name, col]);
        g.strokeStyle = col; g.fillStyle = col; g.lineWidth = 2 * s.dpr;
        if (type === "bar") {
          var bw = Math.max(1, (s.w - padL - padR) / Math.max(pts.length, 1) * 0.7);
          pts.forEach(function (pt) { var y = Y(pt.v), y0 = Y(Math.max(lo, 0)); g.fillRect(X(pt.t) - bw / 2, Math.min(y, y0), bw, Math.abs(y0 - y)); });
          return;
        }
        g.beginPath();
        pts.forEach(function (pt, k) {
          var x = X(pt.t), y = Y(pt.v);
          if (k === 0) g.moveTo(x, y);
          else if (p.interpolate === "step") { g.lineTo(x, Y(pts[k - 1].v)); g.lineTo(x, y); }
          else g.lineTo(x, y);
        });
        g.stroke();
        if (String(p.dot) === "true") pts.forEach(function (pt) { g.beginPath(); g.arc(X(pt.t), Y(pt.v), 2.5 * s.dpr, 0, 7); g.fill(); });
      });
      legend.textContent = "";
      names.forEach(function (n) {
        var item = el("span"), sw = el("i"); sw.style.background = n[1];
        item.append(sw, document.createTextNode(n[0] || "value")); legend.appendChild(item);
      });
    }
    function redraw() { if (!pending) { pending = true; requestAnimationFrame(draw); } }
    new ResizeObserver(redraw).observe(plot);
    return { el: root, set: function (st, upd) {
      if (upd && upd.clear) points = [];
      var add = upd ? (upd.points || []) : (st && st.points || []);
      if (!upd) points = [];
      add.forEach(function (pt) { points.push(pt); });
      if (keepMs > 0 && points.length) {
        var cut = points[points.length - 1].t - keepMs, i = 0;
        while (i < points.length && points[i].t < cut) i++;
        if (i) points = points.slice(i);
      }
      redraw();
    } };
  };

  // ---- template ----------------------------------------------------
  // A ui_template is HTML with a small Angular-style layer: {{ }}, ng-click,
  // ng-class, ng-init, ng-show, ng-hide, ng-if, ng-bind, scope.$watch,
  // scope.send and scope.msg.
  makers.ui_template = function (d) {
    var p = d.props, root = el("div", "w w-template");
    var scopeObj = { msg: {}, $watchers: [] };
    var scope = makeScope(scopeObj);
    var bindings = [];
    var raw = String(p.format || "");
    var scripts = [];
    var html = raw.replace(/<script\b[^>]*>([\s\S]*?)<\/script>/gi, function (_, code) { scripts.push(code); return ""; });
    var holder = el("div"); holder.style.width = "100%"; holder.style.height = "100%";
    holder.innerHTML = html;
    root.appendChild(holder);

    scopeObj.send = function (m) { emit(d.id, { event: "send", msg: m }); };
    scopeObj.$watch = function (expr, fn) {
      var w = { expr: expr, fn: fn, last: undefined, first: true };
      scopeObj.$watchers.push(w);
      return function () { var i = scopeObj.$watchers.indexOf(w); if (i >= 0) scopeObj.$watchers.splice(i, 1); };
    };
    scopeObj.$eval = function (e) { return evalIn(e, scope); };
    scopeObj.$apply = scopeObj.$digest = function () { digest(); };

    function walk(node) {
      if (node.nodeType === 3) {
        if (/\{\{/.test(node.nodeValue)) {
          var tpl = node.nodeValue;
          bindings.push(function () { var v = interpolate(tpl, scope); if (node.nodeValue !== v) node.nodeValue = v; });
        }
        return;
      }
      if (node.nodeType !== 1) return;
      var attrs = Array.prototype.slice.call(node.attributes);
      attrs.forEach(function (a) {
        var n = a.name, v = a.value;
        if (n === "ng-init") runStatement(v, scope);
        else if (n === "ng-click") node.addEventListener("click", function () { runStatement(v, scope); digest(); });
        else if (n === "ng-class") {
          var added = {};
          bindings.push(function () {
            var r = evalIn(v, scope), want = {};
            if (typeof r === "string") r.split(/\s+/).forEach(function (c) { if (c) want[c] = 1; });
            else if (Array.isArray(r)) r.forEach(function (c) { if (c) want[c] = 1; });
            else if (r && typeof r === "object") Object.keys(r).forEach(function (c) { if (r[c]) want[c] = 1; });
            Object.keys(added).forEach(function (c) { if (!want[c]) { node.classList.remove(c); delete added[c]; } });
            Object.keys(want).forEach(function (c) { if (!added[c]) { node.classList.add(c); added[c] = 1; } });
          });
        } else if (n === "ng-show" || n === "ng-if") bindings.push(function () { node.style.display = evalIn(v, scope) ? "" : "none"; });
        else if (n === "ng-hide") bindings.push(function () { node.style.display = evalIn(v, scope) ? "none" : ""; });
        else if (n === "ng-bind") bindings.push(function () { var r = evalIn(v, scope); node.textContent = r == null ? "" : String(r); });
        else if (/\{\{/.test(v) && n.indexOf("ng-") !== 0) {
          bindings.push(function () { var r = interpolate(v, scope); if (node.getAttribute(n) !== r) node.setAttribute(n, r); });
        }
      });
      Array.prototype.slice.call(node.childNodes).forEach(walk);
    }
    walk(holder);

    function digest() {
      for (var pass = 0; pass < 5; pass++) {
        var changed = false;
        scopeObj.$watchers.slice().forEach(function (w) {
          var v = evalIn(w.expr, scope), s = JSON.stringify(v);
          if (w.first || s !== w.last) {
            var old = w.lastVal;
            w.last = s; w.lastVal = v; w.first = false; changed = true;
            try { w.fn(v, old); } catch (e) { console.error("template watcher:", e); }
          }
        });
        bindings.forEach(function (b) { try { b(); } catch (e) { /* a broken binding leaves its text as it was */ } });
        if (!changed) break;
      }
    }
    scripts.forEach(function (code) {
      try { new Function("scope", code)(scopeObj); } catch (e) { console.error("template script:", e); } // eslint-disable-line no-new-func
    });
    digest();
    return { el: root, set: function (st) {
      scopeObj.msg = st && st.msg || {};
      digest();
    } };
  };

  makers.ui_spacer = function () { return { el: el("div", "w w-spacer"), set: function () {} }; };

  // ------------------------------------------------------------------
  // Page
  // ------------------------------------------------------------------
  function widgetSize(p, group) {
    var w = intv(p.width, 0), h = intv(p.height, 0);
    return { w: w > 0 ? Math.min(w, group) : group, h: h > 0 ? h : 1 };
  }

  function build() {
    live.clear();
    page.textContent = "";
    menu.textContent = "";
    var tabs = (layout.tabs || []).filter(function (t) { return !t.props.hidden && !t.props.disabled; });
    var links = layout.links || [];
    $("empty").hidden = tabs.length > 0 || links.length > 0;
    if (!activeTab || !tabs.some(function (t) { return t.id === activeTab; })) {
      var saved = null;
      try { saved = localStorage.getItem("dashboard.tab"); } catch (e) { /* storage may be blocked */ }
      activeTab = tabs.some(function (t) { return t.id === saved; }) ? saved : tabs.length ? tabs[0].id : null;
    }
    var items = [];
    tabs.forEach(function (t) { items.push({ order: t.order, tab: t }); });
    links.forEach(function (l) { items.push({ order: l.order, link: l }); });
    items.sort(function (a, b) { return a.order - b.order; });
    items.forEach(function (it) {
      var a = document.createElement("a");
      if (it.tab) {
        var t = it.tab;
        a.textContent = t.label; a.href = "#" + encodeURIComponent(t.id);
        if (t.id === activeTab) a.setAttribute("aria-current", "page");
        a.addEventListener("click", function (e) { e.preventDefault(); select(t.id); menu.classList.remove("open"); $("menu-btn").setAttribute("aria-expanded", "false"); });
      } else {
        var l = it.link;
        a.textContent = l.name || l.link; a.className = "link";
        var u = null;
        try { u = new URL(l.link, location.href); } catch (e) { /* not a link */ }
        if (u && (u.protocol === "http:" || u.protocol === "https:")) {
          a.href = u.href;
          if (l.target === "newtab") { a.target = "_blank"; a.rel = "noopener"; }
        }
      }
      menu.appendChild(a);
    });
    tabs.forEach(function (t) {
      var sec = el("section", "tab" + (t.id === activeTab ? " active" : ""));
      sec.dataset.tab = t.id;
      (t.groups || []).forEach(function (g) {
        var cols = Math.max(1, intv(g.props.width, 6));
        var box = el("div", "group");
        box.style.width = "calc(" + cols + " * var(--cell) + " + (cols - 1) + " * var(--gap) + 18px)";
        if (String(g.props.disp) !== "false" && g.props.name) {
          var h = el("h2", "", g.props.name);
          if (String(g.props.collapse) === "true") {
            h.className = "toggle"; h.tabIndex = 0; h.setAttribute("role", "button");
            var flip = function () { box.classList.toggle("collapsed"); h.setAttribute("aria-expanded", box.classList.contains("collapsed") ? "false" : "true"); };
            h.addEventListener("click", flip);
            h.addEventListener("keydown", function (e) { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); flip(); } });
          }
          box.appendChild(h);
        }
        var cells = el("div", "cells");
        cells.style.gridTemplateColumns = "repeat(" + cols + ", minmax(0, 1fr))";
        (g.widgets || []).forEach(function (w) {
          var mk = makers[w.type];
          if (!mk) return;
          var made;
          try { made = mk(w); } catch (e) { console.error(w.type, w.id, e); return; }
          var sz = widgetSize(w.props, cols);
          made.el.style.gridColumn = "span " + sz.w;
          made.el.style.gridRow = "span " + sz.h;
          cells.appendChild(made.el);
          live.set(w.id, made);
          var st = state.get(w.id);
          if (st) made.set(st);
        });
        box.appendChild(cells);
        sec.appendChild(box);
      });
      page.appendChild(sec);
    });
    var cur = tabs.filter(function (t) { return t.id === activeTab; })[0];
    titleEl.textContent = cur ? cur.label : "Dashboard";
    document.title = cur ? cur.label : "Dashboard";
    applyTheme(layout.theme || {});
  }

  function applyTheme(t) {
    var root = document.documentElement;
    var dark = typeof t.name === "string" && /dark/i.test(t.name);
    if (t.name) root.dataset.theme = dark ? "dark" : "light";
    var pick = dark ? t.darkTheme : t.lightTheme;
    var c = pick && (pick.baseColor || pick.default);
    if (c && /^#[0-9a-f]{3,8}$/i.test(c)) root.style.setProperty("--primary", c);
  }

  function select(id) {
    activeTab = id;
    try { localStorage.setItem("dashboard.tab", id); } catch (e) { /* storage may be blocked */ }
    build();
  }

  $("menu-btn").addEventListener("click", function () {
    var open = menu.classList.toggle("open");
    $("menu-btn").setAttribute("aria-expanded", open ? "true" : "false");
  });

  // Layout reads can overtake each other during a deploy; only the newest counts.
  var layoutSeq = 0;
  function loadLayout() {
    var mine = ++layoutSeq;
    return fetch(base + "api/layout", { cache: "no-store" }).then(function (r) {
      // With no dashboard node running the server has nothing mounted; that is an empty dashboard, not a broken one.
      return r.ok && /json/.test(r.headers.get("content-type") || "") ? r.json() : { tabs: [], links: [], theme: {} };
    }).then(function (l) {
      if (mine !== layoutSeq) return;
      layout = l; build();
    });
  }

  // ------------------------------------------------------------------
  // The event stream
  // ------------------------------------------------------------------
  var es = null, retry = 1000;
  function connect() {
    conn.dataset.state = "connecting";
    es = new EventSource(base + "api/events");
    es.onopen = function () { conn.dataset.state = "open"; retry = 1000; };
    es.addEventListener("state", function (e) {
      var data = JSON.parse(e.data);
      state.clear();
      (data.states || []).forEach(function (s) { state.set(s.id, s); });
      state.forEach(function (st, id) { var w = live.get(id); if (w) w.set(st); });
    });
    es.addEventListener("update", function (e) {
      (JSON.parse(e.data).updates || []).forEach(function (u) {
        var st = state.get(u.id) || {};
        if (u.msg !== undefined) st.msg = u.msg;
        if (u.value !== undefined) st.value = u.value;
        if (u.clear) st.points = [];
        if (u.points) { st.points = (st.points || []).concat(u.points); if (st.points.length > 5000) st.points = st.points.slice(-5000); }
        state.set(u.id, st);
        var w = live.get(u.id);
        if (w) w.set(st, u);
      });
    });
    es.addEventListener("layout", function () { loadLayout(); });
    es.onerror = function () {
      conn.dataset.state = "lost";
      es.close();
      setTimeout(connect, retry);
      retry = Math.min(retry * 2, 15000);
    };
  }

  loadLayout().then(connect).catch(function () {
    conn.dataset.state = "lost";
    setTimeout(function () { location.reload(); }, 5000);
  });
})();
