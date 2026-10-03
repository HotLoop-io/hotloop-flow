// Writes node-red-4-http.json: what the libraries inside Node-RED 4's HTTP nodes
// do with cookies and multipart bodies, for a set of inputs. The Go port has to
// give the same answer for every one of them.
//
// The versions are the ones Node-RED's package.json pins:
//
//   npm install express@4.22.2 cookie@0.7.2 cookie-parser@1.4.7 \
//       form-data@4.0.6 multer@2.3.0 tough-cookie@5.1.2
//   node golden.mjs > node-red-4-http.json
//
// Where Node-RED's own node code sits between the library and the message, that
// code is copied here verbatim from @node-red/nodes 4.x, 21-httpin.js and
// 21-httprequest.js, and marked.

import { createRequire } from 'node:module'
import http from 'node:http'
import { Writable } from 'node:stream'

const require = createRequire(import.meta.url)
const express = require('express')
const cookie = require('cookie')
const cookieParser = require('cookie-parser')
const FormData = require('form-data')
const multer = require('multer')
const mime = require('mime-types')
const { CookieJar } = require('tough-cookie')

// Object keys in every case are written in sorted order. JavaScript keeps
// insertion order, the Go port sorts, and with the keys already sorted the two
// agree, so the comparison is about what each name becomes and not about map
// order.

// Express reads the clock through Date.now for maxAge. Pinned, so the
// Expires dates are the same on every run.
const NOW = Date.UTC(2026, 9, 3, 12, 0, 0, 250)
Date.now = () => NOW

// A value written as {"$date": ms} is a Date, the way a Function node makes one.
function revive (v) {
  if (Array.isArray(v)) return v.map(revive)
  if (v && typeof v === 'object') {
    if (Object.keys(v).length === 1 && '$date' in v) return new Date(v.$date)
    if (Object.keys(v).length === 1 && '$buffer' in v) return Buffer.from(v.$buffer, 'utf8')
    const out = {}
    for (const k of Object.keys(v)) out[k] = revive(v[k])
    return out
  }
  return v
}

function listen (app) {
  return new Promise(resolve => {
    const srv = app.listen(0, '127.0.0.1', () => resolve(srv))
  })
}

function request (port, opts, body) {
  return new Promise((resolve, reject) => {
    const req = http.request({ host: '127.0.0.1', port, ...opts }, res => {
      const chunks = []
      res.on('data', c => chunks.push(c))
      res.on('end', () => resolve({ status: res.statusCode, headers: res.headers, body: Buffer.concat(chunks) }))
    })
    req.on('error', reject)
    if (body) req.write(body)
    req.end()
  })
}

// ---------------------------------------------------------------------------
// http response: msg.cookies -> Set-Cookie
// ---------------------------------------------------------------------------

const responseCases = [
  { name: 'a plain string', cookies: { session: 'abc123' } },
  { name: 'a number and a boolean', cookies: { count: 42, flag: true } },
  { name: 'a value that needs encoding', cookies: { note: 'line 3; shift=B & co/é' } },
  { name: 'an object value goes out as j: JSON', cookies: { prefs: { value: { line: 3, tags: ['a', 'b'] } } } },
  { name: 'every option', cookies: { full: { value: 'v', domain: 'plant.example.com', path: '/hmi', httpOnly: true, secure: true, sameSite: 'lax', priority: 'High', partitioned: true } } },
  { name: 'sameSite true is Strict', cookies: { s: { value: 'v', sameSite: true } } },
  { name: 'sameSite none', cookies: { s: { value: 'v', sameSite: 'None', secure: true } } },
  { name: 'maxAge in milliseconds', cookies: { m: { value: 'v', maxAge: 900000 } } },
  { name: 'maxAge as a string', cookies: { m: { value: 'v', maxAge: '1500' } } },
  { name: 'a negative maxAge', cookies: { m: { value: 'v', maxAge: -1500 } } },
  { name: 'a fractional maxAge', cookies: { m: { value: 'v', maxAge: 1999.9 } } },
  { name: 'expires from a Date', cookies: { e: { value: 'v', expires: { $date: Date.UTC(2027, 0, 1) } } } },
  { name: 'expires as a string is refused', cookies: { e: { value: 'v', expires: 'Fri, 01 Jan 2027 00:00:00 GMT' } } },
  { name: 'null clears', cookies: { gone: null } },
  { name: 'a null value clears with options', cookies: { gone: { value: null, path: '/hmi', domain: 'plant.example.com' } } },
  { name: 'a null value with maxAge', cookies: { gone: { value: null, maxAge: 1000 } } },
  { name: 'an object with no value', cookies: { u: { path: '/x' } } },
  { name: 'an array', cookies: { arr: [1, 2] } },
  { name: 'path null falls back to /', cookies: { p: { value: 'v', path: null } } },
  { name: 'an empty path is left off', cookies: { p: { value: 'v', path: '' } } },
  { name: 'encode false still encodes', cookies: { enc: { value: 'a b', encode: false } } },
  { name: 'encode true is refused', cookies: { enc: { value: 'a', encode: true } } },
  { name: 'a bad name', cookies: { 'bad name': 'v' } },
  { name: 'a bad domain', cookies: { d: { value: 'v', domain: 'not a domain' } } },
  { name: 'a bad sameSite', cookies: { s: { value: 'v', sameSite: 'sometimes' } } },
  { name: 'a bad priority', cookies: { p: { value: 'v', priority: 'urgent' } } },
  { name: 'a bad maxAge', cookies: { m: { value: 'v', maxAge: 'soon' } } },
  { name: 'signed needs a secret', cookies: { s: { value: 'v', signed: true } } },
  { name: 'several, in name order', cookies: { alpha: '1', beta: { value: '2', path: '/b' }, gamma: null } },
  { name: 'falsy options are left off', cookies: { f: { value: 'v', httpOnly: 0, secure: '', domain: false, sameSite: false } } },
  { name: 'a date as the value', cookies: { d: { value: { $date: Date.UTC(2026, 9, 3) } } } },
  { name: 'unicode in a j: value', cookies: { u: { value: { msg: 'naïve <tag> & "quotes"' } } } }
]

async function goldenResponses () {
  const app = express()
  let current
  app.get('/', (req, res) => {
    const msg = { cookies: revive(current.cookies) }
    try {
      // --- 21-httpin.js, HTTPOut, verbatim ---
      if (msg.cookies) {
        for (var name in msg.cookies) {
          if (msg.cookies.hasOwnProperty(name)) {
            if (msg.cookies[name] === null || msg.cookies[name].value === null) {
              if (msg.cookies[name] !== null) {
                res.clearCookie(name, msg.cookies[name])
              } else {
                res.clearCookie(name)
              }
            } else if (typeof msg.cookies[name] === 'object') {
              res.cookie(name, msg.cookies[name].value, msg.cookies[name])
            } else {
              res.cookie(name, msg.cookies[name])
            }
          }
        }
      }
      // --- end ---
      res.set('x-ok', '1').end()
    } catch (err) {
      res.set('x-error', err.message).end()
    }
  })
  const srv = await listen(app)
  const out = []
  for (const c of responseCases) {
    current = c
    const r = await request(srv.address().port, { path: '/' })
    out.push(r.headers['x-error'] !== undefined
      ? { ...c, error: r.headers['x-error'] }
      : { ...c, setCookie: r.headers['set-cookie'] || [] })
  }
  srv.close()
  return out
}

// ---------------------------------------------------------------------------
// http in: Cookie -> msg.req.cookies
// ---------------------------------------------------------------------------

const requestHeaderCases = [
  'session=abc123',
  'a=1; b=2; c=3',
  'a=1;b=2',
  '  spaced = value  ; x=y',
  'dup=first; dup=second',
  'quoted="hello world"',
  'enc=line%203%3B%20shift%3DB',
  'bad=%E0%A4%A',
  'json=j%3A%7B%22line%22%3A3%7D',
  'jnull=j%3Anull; jzero=j%3A0; jfalse=j%3Afalse; jempty=j%3A%22%22',
  'jbad=j%3A%7Bnope',
  'flag; after=1',
  '=nokey; k=v',
  'a=b=c',
  'empty=; next=1',
  'x="',
  'tab\t=\tv'
]

async function goldenRequestHeaders () {
  const app = express()
  app.get('/', cookieParser(), (req, res) => res.json({ cookies: req.cookies, signedCookies: req.signedCookies }))
  const srv = await listen(app)
  const out = []
  for (const header of requestHeaderCases) {
    const r = await request(srv.address().port, { path: '/', headers: { Cookie: header } })
    out.push({ header, ...JSON.parse(r.body.toString()) })
  }
  srv.close()
  return out
}

// ---------------------------------------------------------------------------
// http request: msg.cookies and a cookie header -> the Cookie header sent
// ---------------------------------------------------------------------------

const outboundCases = [
  { name: 'plain values', url: 'http://127.0.0.1/api/data', cookies: { a: '1', b: 'two words' } },
  { name: 'an object value', url: 'http://127.0.0.1/', cookies: { a: { value: 'x;y' } } },
  { name: 'encode false', url: 'http://127.0.0.1/', cookies: { a: { value: 'plain', encode: false } } },
  { name: 'encode false with a bad value', url: 'http://127.0.0.1/', cookies: { a: { value: 'a b', encode: false } } },
  { name: 'nulls are skipped', url: 'http://127.0.0.1/', cookies: { a: null, b: { value: null }, c: '3' } },
  { name: 'a number, a boolean, undefined and an object', url: 'http://127.0.0.1/', cookies: { n: 7, o: { value: { k: 1 } }, t: true, u: { path: '/' } } },
  { name: 'from a cookie header', url: 'http://127.0.0.1/', header: 'h1=v1; h2=v2' },
  { name: 'header then msg.cookies, same name replaced in place', url: 'http://127.0.0.1/', header: 'a=fromHeader; z=26', cookies: { a: 'fromMsg', b: '2' } },
  { name: 'a header cookie that does not serialize', url: 'http://127.0.0.1/', header: 'bad=a b' },
  { name: 'a quoted header value', url: 'http://127.0.0.1/', header: 'q="quoted"' },
  { name: 'a bad name', url: 'http://127.0.0.1/', cookies: { 'bad name': 'v' } },
  { name: 'an array', url: 'http://127.0.0.1/', cookies: { arr: ['x'] } },
  { name: 'a quoted value with encode false', url: 'http://127.0.0.1/', cookies: { q: { value: '"kept"', encode: false } } },
  { name: 'a path sets the default cookie path', url: 'http://127.0.0.1/api/v1/readings', cookies: { a: '1' } }
]

function goldenOutbound () {
  return outboundCases.map(c => {
    const url = c.url
    const opts = { headers: {}, cookieJar: new CookieJar() }
    if (c.header !== undefined) opts.headers.cookie = c.header
    const msg = { cookies: c.cookies }
    try {
      // --- 21-httprequest.js, verbatim ---
      if (opts.headers.hasOwnProperty('cookie')) {
        var cookies = cookie.parse(opts.headers.cookie, { decode: String })
        for (var name in cookies) {
          opts.cookieJar.setCookieSync(cookie.serialize(name, cookies[name], { encode: String }), url, { ignoreError: true })
        }
        delete opts.headers.cookie
      }
      if (msg.cookies) {
        for (var name in msg.cookies) {
          if (msg.cookies.hasOwnProperty(name)) {
            if (msg.cookies[name] === null || msg.cookies[name].value === null) {
              // This case clears a cookie for HTTP In/Response nodes.
              // Ignore for this node.
            } else if (typeof msg.cookies[name] === 'object') {
              if (msg.cookies[name].encode === false) {
                // If the encode option is false, the value is not encoded.
                opts.cookieJar.setCookieSync(cookie.serialize(name, msg.cookies[name].value, { encode: String }), url, { ignoreError: true })
              } else {
                // The value is encoded by encodeURIComponent().
                opts.cookieJar.setCookieSync(cookie.serialize(name, msg.cookies[name].value), url, { ignoreError: true })
              }
            } else {
              opts.cookieJar.setCookieSync(cookie.serialize(name, msg.cookies[name]), url, { ignoreError: true })
            }
          }
        }
      }
      // --- end ---
      // What got puts on the request.
      return { ...c, cookieHeader: opts.cookieJar.getCookieStringSync(url) }
    } catch (err) {
      return { ...c, error: err.message }
    }
  })
}

// ---------------------------------------------------------------------------
// http request: Set-Cookie -> msg.responseCookies
// ---------------------------------------------------------------------------

// --- 21-httprequest.js, extractCookies, verbatim ---
function extractCookies (setCookie) {
  var cookies = {}
  if (!Array.isArray(setCookie)) {
    return cookies
  }
  setCookie.forEach(function (c) {
    try {
      if (typeof c !== 'string') {
        return
      }
      var parsedCookie = cookie.parse(c)
      var eq_idx = c.indexOf('=')
      if (eq_idx === -1) {
        return
      }
      var key = c.substr(0, eq_idx).trim()
      if (!key) {
        return
      }
      parsedCookie.value = parsedCookie[key]
      delete parsedCookie[key]
      cookies[key] = parsedCookie
    } catch (err) {
      // Skip malformed cookies
    }
  })
  return cookies
}
// --- end ---

const setCookieCases = [
  ['sid=abc; Path=/; HttpOnly'],
  ['sid=abc%20d; Path=/hmi; Expires=Wed, 21 Oct 2026 07:28:00 GMT; Secure; SameSite=Lax', 'theme=dark'],
  ['a=1', 'a=2'],
  ['value=oops; Path=/'],
  ['noequals', 'k=v'],
  ['  spaced  =  v  ; Max-Age=60'],
  ['q="quoted"; Domain=plant.example.com'],
  ['bad=%E0%A4%A; Path=/']
]

// ---------------------------------------------------------------------------
// http request: an object payload -> a multipart/form-data body
// ---------------------------------------------------------------------------

const formCases = [
  { name: 'strings', payload: { line: '3', note: 'shift B' } },
  { name: 'a buffer', payload: { blob: { $buffer: 'raw bytes' } } },
  { name: 'a file with a filename', payload: { report: { value: 'a,b\n1,2\n', options: { filename: 'report.csv' } } } },
  { name: 'a buffer file with a content type', payload: { img: { value: { $buffer: 'PNG...' }, options: { filename: 'shot.png', contentType: 'image/x-custom' } } } },
  { name: 'a filename as the options string', payload: { f: { value: 'x', options: 'name.txt' } } },
  { name: 'an unknown extension on a string', payload: { f: { value: 'x', options: { filename: 'data.qqq' } } } },
  { name: 'an unknown extension on a buffer', payload: { f: { value: { $buffer: 'x' }, options: { filename: 'data.qqq' } } } },
  { name: 'a path in the filename', payload: { f: { value: 'x', options: { filename: '/var/data/plant/readings.json' } } } },
  { name: 'numbers, booleans, arrays and objects as JSON', payload: { arr: [1, 'two'], b: true, n: 5, obj: { k: 'v' } } },
  { name: 'null and undefined are left out', payload: { gone: null, keep: 'yes' } },
  { name: 'a value that is a number', payload: { n: { value: 12 } } },
  { name: 'a value that is null', payload: { n: { value: null } } },
  { name: 'quotes and newlines in names are escaped', payload: { 'we"ird\nname': { value: 'v', options: { filename: 'a"b\r.txt' } } } },
  { name: 'a custom header object', payload: { h: { value: 'v', options: { header: { 'X-Line': '3', 'Content-Type': 'ignored/type' } } } } },
  { name: 'a custom header string', payload: { h: { value: 'v', options: { header: '--BOUNDARY\r\nContent-Disposition: form-data; name="custom"\r\n\r\n' } } } },
  { name: 'an array value is refused', payload: { a: { value: [1, 2] } } },
  { name: 'an empty object sends nothing', payload: {} },
  { name: 'an array payload', payload: ['first', 'second'] }
]

function goldenForms () {
  return Promise.all(formCases.map(c => new Promise(resolve => {
    const msg = { payload: revive(c.payload) }
    let failed = null
    // --- 21-httprequest.js, verbatim, except that the form's error event has
    // a listener. In Node-RED it has none, so an append that fails throws out
    // of the node's input handler and the request is never sent.
    let formData = new FormData()
    formData.on('error', err => { failed = failed || err.message })
    // A fixed boundary, set before the first append writes it into a part.
    formData.setBoundary('BOUNDARY')
    for (var opt in msg.payload) {
      if (msg.payload.hasOwnProperty(opt)) {
        var val = msg.payload[opt]
        if (val !== undefined && val !== null) {
          if (typeof val === 'string' || Buffer.isBuffer(val)) {
            formData.append(opt, val)
          } else if (typeof val === 'object' && val.hasOwnProperty('value')) {
            formData.append(opt, val.value, val.options || {})
          } else {
            formData.append(opt, JSON.stringify(val))
          }
        }
      }
    }
    // --- end ---
    if (failed) return resolve({ ...c, error: failed })
    // Streamed, the way got sends it.
    const chunks = []
    const sink = new Writable({
      write (chunk, enc, cb) { chunks.push(Buffer.from(chunk)); cb() }
    })
    sink.on('finish', () => resolve(failed
      ? { ...c, error: failed }
      : { ...c, body: Buffer.concat(chunks).toString('latin1') }))
    sink.on('error', err => resolve({ ...c, error: err.message }))
    try {
      formData.pipe(sink)
    } catch (err) {
      resolve({ ...c, error: err.message })
    }
  })))
}

// ---------------------------------------------------------------------------
// http in: a multipart upload -> msg.payload and msg.req.files
// ---------------------------------------------------------------------------

const CRLF = '\r\n'
function part (headers, body) {
  return '--XB' + CRLF + headers.join(CRLF) + CRLF + CRLF + body + CRLF
}
const END = '--XB--' + CRLF

const uploadCases = [
  { name: 'fields and a file', body: part(['Content-Disposition: form-data; name="line"'], '3') + part(['Content-Disposition: form-data; name="csv"; filename="r.csv"', 'Content-Type: text/csv'], 'a,b\n1,2') + END },
  { name: 'a repeated field becomes an array', body: part(['Content-Disposition: form-data; name="tag"'], 'a') + part(['Content-Disposition: form-data; name="tag"'], 'b') + part(['Content-Disposition: form-data; name="tag"'], 'c') + END },
  { name: 'bracket names nest', body: part(['Content-Disposition: form-data; name="line[3][temp]"'], '71.5') + part(['Content-Disposition: form-data; name="line[3][state]"'], 'run') + part(['Content-Disposition: form-data; name="tags[]"'], 'x') + part(['Content-Disposition: form-data; name="tags[]"'], 'y') + END },
  { name: 'array indices, sparse', body: part(['Content-Disposition: form-data; name="a[2]"'], 'two') + part(['Content-Disposition: form-data; name="a[0]"'], 'zero') + END },
  { name: 'an array turns into an object', body: part(['Content-Disposition: form-data; name="a[0]"'], 'zero') + part(['Content-Disposition: form-data; name="a[x]"'], 'ex') + END },
  { name: 'a scalar then a nested key', body: part(['Content-Disposition: form-data; name="a"'], 'one') + part(['Content-Disposition: form-data; name="a[b]"'], 'two') + END },
  { name: 'a malformed path is a literal key', body: part(['Content-Disposition: form-data; name="a[b"'], 'v') + part(['Content-Disposition: form-data; name="[x]"'], 'w') + part(['Content-Disposition: form-data; name="a[]b"'], 'z') + END },
  { name: 'octet-stream without a filename is dropped', body: part(['Content-Disposition: form-data; name="blob"', 'Content-Type: application/octet-stream'], 'xx') + part(['Content-Disposition: form-data; name="kept"'], 'k') + END },
  { name: 'an empty filename is dropped', body: part(['Content-Disposition: form-data; name="f"; filename=""'], 'xx') + END },
  { name: 'a path in the filename is stripped', body: part(['Content-Disposition: form-data; name="f"; filename="C:\\\\plant\\\\line3\\\\r.txt"'], 'x') + part(['Content-Disposition: form-data; name="g"; filename="../../etc/passwd"'], 'y') + END },
  { name: 'filename* wins', body: part(["Content-Disposition: form-data; name=\"f\"; filename=\"plain.txt\"; filename*=UTF-8''%E2%82%AC%20rates.txt"], 'x') + END },
  { name: 'a raw UTF-8 filename is read as latin1', body: part(['Content-Disposition: form-data; name="f"; filename="café.txt"'], 'x') + END },
  { name: 'escaped quote and newline in a filename', body: part(['Content-Disposition: form-data; name="f"; filename="a%22b%0Ac.txt"'], 'x') + END },
  { name: 'a backslash-escaped quote', body: part(['Content-Disposition: form-data; name="f"; filename="say \\"hi\\".txt"'], 'x') + END },
  { name: 'a field charset of latin1', body: part(['Content-Disposition: form-data; name="t"', 'Content-Type: text/plain; charset=iso-8859-1'], 'caf\u00e9') + END, latin1: true },
  { name: 'transfer encoding is reported, not applied', body: part(['Content-Disposition: form-data; name="f"; filename="b.bin"', 'Content-Type: application/x-thing', 'Content-Transfer-Encoding: BASE64'], 'aGk=') + END },
  { name: 'a part with no disposition is skipped', body: part(['Content-Type: text/plain'], 'lost') + part(['Content-Disposition: form-data; name="k"'], 'v') + END },
  { name: 'an attachment disposition is skipped', body: part(['Content-Disposition: attachment; name="k"'], 'lost') + END },
  { name: 'a field with no name', body: part(['Content-Disposition: form-data; filler=1'], 'v') + END },
  { name: 'a file with no name', body: part(['Content-Disposition: form-data; filename="x.txt"'], 'v') + END },
  { name: 'no closing boundary', body: part(['Content-Disposition: form-data; name="k"'], 'v') },
  { name: 'a quoted boundary', body: part(['Content-Disposition: form-data; name="k"'], 'v') + END, contentType: 'multipart/form-data; boundary="XB"' },
  { name: 'no boundary', body: part(['Content-Disposition: form-data; name="k"'], 'v') + END, contentType: 'multipart/form-data' },
  { name: 'multipart/mixed is unsupported', body: part(['Content-Disposition: form-data; name="k"'], 'v') + END, contentType: 'multipart/mixed; boundary=XB' },
  { name: 'no fields at all', body: END }
]

async function goldenUploads () {
  const app = express()
  const mp = multer({ storage: multer.memoryStorage() }).any()
  // --- 21-httpin.js: the multipart parser and error handler, verbatim ---
  const multipartParser = function (req, res, next) {
    if (req.readableEnded || req._body) return next()
    mp(req, res, function (err) {
      req._body = true
      next(err)
    })
  }
  const errorHandler = function (err, req, res, next) {
    res.set('x-error', err.message)
    res.sendStatus(500)
  }
  // --- end ---
  app.post('/', multipartParser, (req, res) => {
    res.json({
      payload: req.body,
      files: (req.files || []).map(f => ({ ...f, buffer: f.buffer.toString('base64') }))
    })
  }, errorHandler)
  const srv = await listen(app)
  const out = []
  for (const c of uploadCases) {
    const body = Buffer.from(c.body, c.latin1 ? 'latin1' : 'utf8')
    const r = await request(srv.address().port, {
      method: 'POST',
      path: '/',
      headers: { 'Content-Type': c.contentType || 'multipart/form-data; boundary=XB', 'Content-Length': body.length }
    }, body)
    const rec = { name: c.name, contentType: c.contentType || 'multipart/form-data; boundary=XB', body: body.toString('base64') }
    if (r.status !== 200) {
      out.push({ ...rec, status: r.status, error: r.headers['x-error'] })
    } else {
      out.push({ ...rec, ...JSON.parse(r.body.toString()) })
    }
  }
  srv.close()
  return out
}

const mimeExtensions = [
  '7z', 'avif', 'bin', 'bmp', 'bz2', 'cer', 'crt', 'css', 'csv', 'der', 'doc', 'docx', 'dxf',
  'gif', 'gz', 'htm', 'html', 'ico', 'ini', 'jpeg', 'jpg', 'js', 'json', 'log', 'md', 'mjs',
  'mp3', 'mp4', 'ods', 'ogg', 'p12', 'pdf', 'pem', 'pfx', 'png', 'ppt', 'pptx', 'svg', 'tar',
  'tif', 'tiff', 'toml', 'txt', 'wasm', 'wav', 'webm', 'webp', 'xls', 'xlsx', 'xml', 'yaml',
  'yml', 'zip', 'qqq', 'TXT', 'archive.tar.gz', 'noext'
]

const out = {
  generator: 'express 4.22.2, cookie 0.7.2, cookie-parser 1.4.7, form-data 4.0.6, multer 2.3.0, tough-cookie 5.1.2, mime-types ' + require('mime-types/package.json').version,
  now: NOW,
  responseCookies: await goldenResponses(),
  requestCookies: await goldenRequestHeaders(),
  outboundCookies: goldenOutbound(),
  responseSetCookies: setCookieCases.map(sc => ({ setCookie: sc, cookies: extractCookies(sc) })),
  forms: await goldenForms(),
  uploads: await goldenUploads(),
  mime: Object.fromEntries(mimeExtensions.map(e => [e, mime.lookup(e)]))
}
process.stdout.write(JSON.stringify(out, null, 1) + '\n')
