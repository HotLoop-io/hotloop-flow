(function (global) {
  "use strict";
  var U8 = Uint8Array;

  function Buffer(arg, enc) {
    return Buffer.from(arg, enc);
  }
  Buffer.prototype = Object.create(U8.prototype, {
    constructor: { value: Buffer, writable: true, configurable: true },
  });
  Object.setPrototypeOf(Buffer, U8);
  // subarray and friends make a plain Uint8Array, which wrap() then turns into a Buffer.
  Object.defineProperty(Buffer, Symbol.species, { get: function () { return U8; }, configurable: true });

  function wrap(u8) {
    Object.setPrototypeOf(u8, Buffer.prototype);
    return u8;
  }
  function view(b) {
    return new DataView(b.buffer, b.byteOffset, b.byteLength);
  }
  function rangeError(what, off, len) {
    var e = new RangeError('The value of "offset" is out of range for ' + what + ". Received " + off + ", buffer length " + len);
    e.code = "ERR_OUT_OF_RANGE";
    return e;
  }
  function check(b, off, n, what) {
    if (off === undefined) off = 0;
    if (typeof off !== "number" || off !== Math.floor(off) || off < 0 || off + n > b.length) throw rangeError(what, off, b.length);
    return off;
  }

  function utf8Bytes(s) {
    var out = [];
    for (var i = 0; i < s.length; i++) {
      var c = s.charCodeAt(i);
      if (c >= 0xd800 && c <= 0xdbff && i + 1 < s.length) {
        var d = s.charCodeAt(i + 1);
        if (d >= 0xdc00 && d <= 0xdfff) {
          c = 0x10000 + ((c - 0xd800) << 10) + (d - 0xdc00);
          i++;
        }
      }
      if (c < 0x80) out.push(c);
      else if (c < 0x800) out.push(0xc0 | (c >> 6), 0x80 | (c & 63));
      else if (c < 0x10000) out.push(0xe0 | (c >> 12), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
      else out.push(0xf0 | (c >> 18), 0x80 | ((c >> 12) & 63), 0x80 | ((c >> 6) & 63), 0x80 | (c & 63));
    }
    return out;
  }
  function utf8String(b, start, end) {
    var s = "";
    for (var i = start; i < end; ) {
      var c = b[i++], cp;
      if (c < 0x80) cp = c;
      else if (c >= 0xc0 && c < 0xe0 && i < end) cp = ((c & 31) << 6) | (b[i++] & 63);
      else if (c >= 0xe0 && c < 0xf0 && i + 1 < end) { cp = ((c & 15) << 12) | ((b[i] & 63) << 6) | (b[i + 1] & 63); i += 2; }
      else if (c >= 0xf0 && i + 2 < end) { cp = ((c & 7) << 18) | ((b[i] & 63) << 12) | ((b[i + 1] & 63) << 6) | (b[i + 2] & 63); i += 3; }
      else cp = 0xfffd;
      if (cp >= 0x10000) { cp -= 0x10000; s += String.fromCharCode(0xd800 + (cp >> 10), 0xdc00 + (cp & 1023)); }
      else s += String.fromCharCode(cp);
    }
    return s;
  }
  var B64 = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
  function b64Encode(b, start, end) {
    var s = "";
    for (var i = start; i < end; i += 3) {
      var n = (b[i] << 16) | ((i + 1 < end ? b[i + 1] : 0) << 8) | (i + 2 < end ? b[i + 2] : 0);
      s += B64[(n >> 18) & 63] + B64[(n >> 12) & 63] + (i + 1 < end ? B64[(n >> 6) & 63] : "=") + (i + 2 < end ? B64[n & 63] : "=");
    }
    return s;
  }
  function b64Decode(str) {
    str = String(str).replace(/-/g, "+").replace(/_/g, "/").replace(/[^A-Za-z0-9+\/]/g, "");
    var out = [];
    var acc = 0, bits = 0;
    for (var i = 0; i < str.length; i++) {
      acc = (acc << 6) | B64.indexOf(str[i]);
      bits += 6;
      if (bits >= 8) { bits -= 8; out.push((acc >> bits) & 255); acc &= (1 << bits) - 1; }
    }
    return out;
  }
  function hexDecode(str) {
    var out = [];
    for (var i = 0; i + 1 < str.length; i += 2) {
      var v = parseInt(str.substr(i, 2), 16);
      if (isNaN(v)) break;
      out.push(v);
    }
    return out;
  }
  function fromString(str, enc) {
    enc = (enc || "utf8").toLowerCase();
    switch (enc) {
      case "utf8": case "utf-8": return utf8Bytes(str);
      case "hex": return hexDecode(str);
      case "base64": case "base64url": return b64Decode(str);
      case "ascii": case "latin1": case "binary": { var o = []; for (var i = 0; i < str.length; i++) o.push(str.charCodeAt(i) & 255); return o; }
    }
    var e = new TypeError("Unknown encoding: " + enc);
    e.code = "ERR_UNKNOWN_ENCODING";
    throw e;
  }

  Buffer.alloc = function (size, fill, enc) {
    if (typeof size !== "number" || size < 0) throw new RangeError('The value of "size" is out of range. Received ' + size);
    var b = wrap(new U8(size));
    if (fill !== undefined && fill !== 0) b.fill(typeof fill === "string" ? fromString(fill, enc)[0] || 0 : fill);
    return b;
  };
  Buffer.allocUnsafe = Buffer.alloc;
  Buffer.from = function (v, a, b) {
    if (typeof v === "string") return wrap(U8.from(fromString(v, a)));
    if (v instanceof ArrayBuffer) return wrap(new U8(v, a || 0, b === undefined ? v.byteLength - (a || 0) : b));
    if (v && typeof v === "object" && v.type === "Buffer" && Array.isArray(v.data)) v = v.data;
    if (v && typeof v === "object" && typeof v.length === "number") {
      var out = new U8(v.length);
      for (var i = 0; i < v.length; i++) out[i] = v[i] & 255;
      return wrap(out);
    }
    var e = new TypeError("The first argument must be of type string or an instance of Buffer, ArrayBuffer, or Array or an Array-like Object. Received " + typeof v);
    e.code = "ERR_INVALID_ARG_TYPE";
    throw e;
  };
  Buffer.isBuffer = function (v) { return v instanceof Buffer; };
  Buffer.byteLength = function (s, enc) { return typeof s === "string" ? fromString(s, enc).length : s.length; };
  Buffer.concat = function (list, total) {
    var n = 0, i;
    for (i = 0; i < list.length; i++) n += list[i].length;
    if (total !== undefined) n = total;
    var out = Buffer.alloc(n), pos = 0;
    for (i = 0; i < list.length && pos < n; i++) {
      for (var j = 0; j < list[i].length && pos < n; j++) out[pos++] = list[i][j];
    }
    return out;
  };
  Buffer.compare = function (a, b) {
    var n = Math.min(a.length, b.length);
    for (var i = 0; i < n; i++) if (a[i] !== b[i]) return a[i] < b[i] ? -1 : 1;
    return a.length === b.length ? 0 : a.length < b.length ? -1 : 1;
  };

  var P = Buffer.prototype;
  function def(name, fn) { Object.defineProperty(P, name, { value: fn, writable: true, configurable: true }); }

  function rw(name, size, getter, setter) {
    def("read" + name, function (off) { off = check(this, off, size, "read" + name); return getter(view(this), off); });
    def("write" + name, function (v, off) { off = check(this, off, size, "write" + name); setter(view(this), off, v); return off + size; });
  }
  [["BE", false], ["LE", true]].forEach(function (e) {
    var sfx = e[0], le = e[1];
    rw("UInt16" + sfx, 2, function (d, o) { return d.getUint16(o, le); }, function (d, o, v) { d.setUint16(o, v, le); });
    rw("Int16" + sfx, 2, function (d, o) { return d.getInt16(o, le); }, function (d, o, v) { d.setInt16(o, v, le); });
    rw("UInt32" + sfx, 4, function (d, o) { return d.getUint32(o, le); }, function (d, o, v) { d.setUint32(o, v, le); });
    rw("Int32" + sfx, 4, function (d, o) { return d.getInt32(o, le); }, function (d, o, v) { d.setInt32(o, v, le); });
    rw("Float" + sfx, 4, function (d, o) { return d.getFloat32(o, le); }, function (d, o, v) { d.setFloat32(o, v, le); });
    rw("Double" + sfx, 8, function (d, o) { return d.getFloat64(o, le); }, function (d, o, v) { d.setFloat64(o, v, le); });
    // The Uint spelling Node also accepts.
    ["16", "32"].forEach(function (w) {
      def("readUint" + w + sfx, P["readUInt" + w + sfx]);
      def("writeUint" + w + sfx, P["writeUInt" + w + sfx]);
    });
  });
  def("readUInt8", function (off) { off = check(this, off, 1, "readUInt8"); return this[off]; });
  def("readUint8", P.readUInt8);
  def("readInt8", function (off) { off = check(this, off, 1, "readInt8"); var v = this[off]; return v > 127 ? v - 256 : v; });
  def("writeUInt8", function (v, off) { off = check(this, off, 1, "writeUInt8"); this[off] = v; return off + 1; });
  def("writeUint8", P.writeUInt8);
  def("writeInt8", function (v, off) { off = check(this, off, 1, "writeInt8"); this[off] = v & 255; return off + 1; });

  def("slice", function (start, end) { return wrap(U8.prototype.subarray.call(this, start, end)); });
  def("subarray", P.slice);
  def("toString", function (enc, start, end) {
    start = start === undefined ? 0 : start; end = end === undefined ? this.length : Math.min(end, this.length);
    enc = (enc || "utf8").toLowerCase();
    var i, s = "";
    switch (enc) {
      case "utf8": case "utf-8": return utf8String(this, start, end);
      case "hex": for (i = start; i < end; i++) s += (this[i] < 16 ? "0" : "") + this[i].toString(16); return s;
      case "base64": return b64Encode(this, start, end);
      case "base64url": return b64Encode(this, start, end).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
      case "ascii": case "latin1": case "binary": for (i = start; i < end; i++) s += String.fromCharCode(this[i]); return s;
    }
    var e = new TypeError("Unknown encoding: " + enc); e.code = "ERR_UNKNOWN_ENCODING"; throw e;
  });
  def("toJSON", function () { return { type: "Buffer", data: Array.prototype.slice.call(this) }; });
  def("equals", function (o) { return Buffer.compare(this, o) === 0; });
  def("copy", function (target, ts, ss, se) {
    ts = ts || 0; ss = ss || 0; se = se === undefined ? this.length : se;
    var n = 0;
    for (var i = ss; i < se && ts + n < target.length; i++, n++) target[ts + n] = this[i];
    return n;
  });
  def("write", function (str, off, len, enc) {
    if (typeof off === "string") { enc = off; off = 0; len = undefined; }
    off = off || 0;
    var bytes = fromString(str, enc), n = Math.min(bytes.length, this.length - off, len === undefined ? Infinity : len);
    for (var i = 0; i < n; i++) this[off + i] = bytes[i];
    return n;
  });
  def("swap16", function () { for (var i = 0; i + 1 < this.length; i += 2) { var t = this[i]; this[i] = this[i + 1]; this[i + 1] = t; } return this; });
  def("swap32", function () { for (var i = 0; i + 3 < this.length; i += 4) { var a = this[i], b = this[i + 1]; this[i] = this[i + 3]; this[i + 1] = this[i + 2]; this[i + 2] = b; this[i + 3] = a; } return this; });

  global.Buffer = Buffer;
})(this);
