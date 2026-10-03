// Ported from:
//   - form-data 4.0.6 lib/form_data.js (MIT), Copyright (c) 2012 Felix
//     Geisendörfer and contributors
//   - busboy 1.6.0 lib/utils.js and lib/types/multipart.js (MIT), Copyright
//     Brian White
//   - multer 2.3.0 lib/make-middleware.js and lib/multer-error.js (MIT),
//     Copyright (c) 2014 Hage Yaapa
//   - append-field 1.0.0 lib/parse-path.js and lib/set-value.js (MIT),
//     Copyright (c) 2015 Linus Unnebäck
//   - the mimeTypes table: mime-types 2.1.35 and mime-db 1.52.0 (MIT),
//     Copyright (c) 2014 Jonathan Ong, Copyright (c) 2015-2022 Douglas
//     Christopher Wilson
//   - Node-RED 5.0.7 @node-red/nodes core/network/21-httprequest.js, the
//     multipart/form-data body (Apache-2.0), Copyright JS Foundation and other
//     contributors, http://js.foundation
//
// Modified. The MIT notices are in NOTICE.

package nodes

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"path"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// multipart/form-data both ways, the way Node-RED does it. Out, the HTTP
// Request node builds the body with form-data 4.0.6; in, the HTTP In node
// parses an upload with multer 2.3.0, which is busboy 1.6 underneath, and files
// the text fields with append-field. This ports the parts of each that decide
// what goes on the wire and what lands in the message, and the golden file in
// testdata is the real libraries' output for the same input.

// ---------------------------------------------------------------------------
// out: an object payload as a form
// ---------------------------------------------------------------------------

// newFormBoundary is form-data's: 26 dashes and 24 hex digits.
func newFormBoundary() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return strings.Repeat("-", 26) + hex.EncodeToString(b[:])
}

// buildFormData encodes a payload as Node-RED's HTTP Request node does when the
// content type is multipart/form-data: each property a part, a string or bytes
// as they are, an object with a value property as that value with the object's
// options, and anything else as its JSON. null and undefined properties are
// left out.
//
// Properties go in sorted order, for the reason responseCookies gives.
func buildFormData(payload any, boundary string) ([]byte, error) {
	var fields []string
	var get func(string) any
	switch t := payload.(type) {
	case map[string]any:
		for k := range t {
			fields = append(fields, k)
		}
		sort.Strings(fields)
		get = func(k string) any { return t[k] }
	case []any:
		// for...in over an array walks its indices.
		for i := range t {
			fields = append(fields, strconv.Itoa(i))
		}
		get = func(k string) any { i, _ := strconv.Atoi(k); return t[i] }
	case []byte:
		return buildFormData(bytesAsIndices(t), boundary)
	case engine.ImmutableBytes:
		// So does for...in over a Buffer, and each byte is a number. Nobody
		// means this, and it is what Node-RED sends.
		return buildFormData(bytesAsIndices(t), boundary)
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("a multipart/form-data body needs an object payload")
	}

	var body bytes.Buffer
	parts := 0
	for _, field := range fields {
		val := get(field)
		if val == nil {
			continue
		}
		var (
			value   any
			options map[string]any
		)
		obj, isObj := val.(map[string]any)
		switch {
		case isString(val) || isBytes(val):
			value = val
		case isObj && hasKey(obj, "value"):
			value = obj["value"]
			switch o := obj["options"].(type) {
			case map[string]any:
				options = o
			case string:
				// A string option is the filename.
				options = map[string]any{"filename": o}
			}
		default:
			value = jsJSONStringify(val)
		}
		if err := writeFormPart(&body, boundary, field, value, options); err != nil {
			return nil, err
		}
		parts++
	}
	if parts > 0 {
		// Streamed, as got sends it, the closing boundary rides on the last
		// part's footer, so a form with no parts has no body at all.
		body.WriteString("--" + boundary + "--\r\n")
	}
	return body.Bytes(), nil
}

// bytesAsIndices is a Buffer seen by for...in: index to byte value.
func bytesAsIndices(p []byte) []any {
	out := make([]any, len(p))
	for i, c := range p {
		out[i] = float64(c)
	}
	return out
}

func isString(v any) bool { _, ok := v.(string); return ok }

func isBytes(v any) bool {
	switch v.(type) {
	case []byte, engine.ImmutableBytes:
		return true
	}
	return false
}

// writeFormPart is form-data's append, its _multiPartHeader and the footer.
func writeFormPart(body *bytes.Buffer, boundary, field string, value any, options map[string]any) error {
	var data []byte
	switch t := value.(type) {
	case string:
		data = []byte(t)
	case []byte:
		data = t
	case engine.ImmutableBytes:
		data = t
	case float64, int, nil, jsUndefined:
		// form-data turns a number, null or undefined into its string.
		data = []byte(jsString(t))
	case []any:
		return errors.New("arrays are not supported in a multipart field; " +
			"form-data refuses them too")
	default:
		return fmt.Errorf("the multipart field %q holds a %s; a field takes a string, bytes or a number",
			field, jsTypeName(t))
	}
	if options == nil {
		options = map[string]any{}
	}

	if h, ok := options["header"].(string); ok {
		// A custom header string replaces form-data's, boundary and all.
		body.WriteString(h)
	} else {
		disposition := []string{"form-data", `name="` + escapeHeaderParam(field) + `"`}
		var filename string
		if fp, ok := options["filepath"].(string); ok {
			filename = strings.ReplaceAll(path.Clean(fp), `\`, "/")
		} else if fn := options["filename"]; jsTruthy(fn) {
			filename = jsBasename(jsString(fn))
		}
		if filename != "" {
			disposition = append(disposition, `filename="`+escapeHeaderParam(filename)+`"`)
		}

		var contentType []string
		if ct := options["contentType"]; jsTruthy(ct) {
			contentType = []string{jsString(ct)}
		} else if lookup := firstString(options["filepath"], options["filename"]); lookup != "" {
			if t, ok := mimeLookup(lookup); ok {
				contentType = []string{t}
			}
		}
		if contentType == nil && isBytes(value) {
			contentType = []string{"application/octet-stream"}
		}

		headers := []struct {
			name   string
			values []string
		}{{"Content-Disposition", disposition}, {"Content-Type", contentType}}
		if custom, ok := options["header"].(map[string]any); ok {
			// populate(): a custom header only fills a name form-data left
			// unset, and both of its own always count as set.
			names := make([]string, 0, len(custom))
			for k := range custom {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, k := range names {
				if k == "Content-Disposition" || k == "Content-Type" {
					continue
				}
				var vals []string
				switch v := custom[k].(type) {
				case []any:
					for _, e := range v {
						vals = append(vals, jsString(e))
					}
				case nil:
					continue
				default:
					vals = []string{jsString(v)}
				}
				headers = append(headers, struct {
					name   string
					values []string
				}{k, vals})
			}
		}

		body.WriteString("--" + boundary + "\r\n")
		for _, h := range headers {
			if len(h.values) > 0 {
				body.WriteString(h.name + ": " + strings.Join(h.values, "; ") + "\r\n")
			}
		}
		body.WriteString("\r\n")
	}
	body.Write(data)
	body.WriteString("\r\n")
	return nil
}

func firstString(vs ...any) string {
	for _, v := range vs {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func jsTypeName(v any) string {
	switch v.(type) {
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", v)
}

// escapeHeaderParam is form-data's: CR, LF and the double quote escaped.
func escapeHeaderParam(s string) string {
	return strings.NewReplacer("\r", "%0D", "\n", "%0A", `"`, "%22").Replace(s)
}

// jsBasename is Node's path.basename on POSIX: the last segment, ignoring
// trailing slashes.
func jsBasename(p string) string {
	p = strings.TrimRight(p, "/")
	if p == "" {
		return ""
	}
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// mimeTypes is mime-types 2.1.35's answer for the extensions a flow actually
// uploads. Its full table is mime-db, two thousand entries mostly for formats
// nobody sends from a plant floor; an extension that is not here gets no
// content type, the same as one mime-db does not know.
var mimeTypes = map[string]string{
	"7z": "application/x-7z-compressed", "avif": "image/avif", "bin": "application/octet-stream",
	"bmp": "image/bmp", "bz2": "application/x-bzip2", "cer": "application/pkix-cert",
	"crt": "application/x-x509-ca-cert", "css": "text/css", "csv": "text/csv",
	"der": "application/x-x509-ca-cert", "doc": "application/msword",
	"docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	"dxf":  "image/vnd.dxf", "gif": "image/gif", "gz": "application/gzip", "htm": "text/html",
	"html": "text/html", "ico": "image/vnd.microsoft.icon", "ini": "text/plain",
	"jpeg": "image/jpeg", "jpg": "image/jpeg", "js": "application/javascript",
	"json": "application/json", "log": "text/plain", "md": "text/markdown",
	"mjs": "application/javascript", "mp3": "audio/mpeg", "mp4": "video/mp4",
	"ods": "application/vnd.oasis.opendocument.spreadsheet", "ogg": "audio/ogg",
	"p12": "application/x-pkcs12", "pdf": "application/pdf", "pem": "application/x-x509-ca-cert",
	"pfx": "application/x-pkcs12", "png": "image/png", "ppt": "application/vnd.ms-powerpoint",
	"pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	"svg":  "image/svg+xml", "tar": "application/x-tar", "tif": "image/tiff",
	"tiff": "image/tiff", "toml": "application/toml", "txt": "text/plain",
	"wasm": "application/wasm", "wav": "audio/wave", "webm": "video/webm", "webp": "image/webp",
	"xls":  "application/vnd.ms-excel",
	"xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	"xml":  "application/xml", "yaml": "text/yaml", "yml": "text/yaml", "zip": "application/zip",
}

// mimeLookup is mime-types' lookup: the extension after the last dot of the
// last path segment, lower-cased; a bare name is its own extension.
func mimeLookup(name string) (string, bool) {
	ext := path.Ext("x." + name)
	t, ok := mimeTypes[strings.ToLower(strings.TrimPrefix(ext, "."))]
	return t, ok
}

// ---------------------------------------------------------------------------
// in: an upload to msg.payload and msg.req.files
// ---------------------------------------------------------------------------

// Busboy's defaults, which multer keeps when Node-RED passes no limits.
const (
	maxUploadFieldBytes = 1 << 20

	// maxUploadArrayIndex bounds a[N] in a field name. append-field makes a
	// sparse array that long, which costs JavaScript nothing; a Go slice that
	// long costs the heap. Multer has the same limit as an option for the same
	// reason, off by default.
	maxUploadArrayIndex = 10000
)

// errUpload is what Node-RED answers a broken upload with: the error is a
// warning in the log and the client gets a bare 500.
type errUpload struct{ msg string }

func (e *errUpload) Error() string { return e.msg }

// parseUpload is multer's any() into memory: text fields into an object the way
// append-field builds one, files into a list of {fieldname, originalname,
// encoding, mimetype, buffer, size}.
func parseUpload(contentType string, body []byte) (map[string]any, []any, error) {
	ct, ok := busboyContentType(latin1(contentType))
	if !ok {
		return nil, nil, &errUpload{"Malformed content type"}
	}
	if ct.typ != "multipart" || ct.subtype != "form-data" {
		return nil, nil, &errUpload{"Unsupported content type: " + contentType}
	}
	boundary, ok := ct.params["boundary"]
	if !ok {
		return nil, nil, &errUpload{"Multipart: Boundary not found"}
	}

	fields := newAFObject()
	files := []any{}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := mr.NextRawPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, &errUpload{"Unexpected end of form"}
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, nil, &errUpload{"Unexpected end of form"}
		}

		dispRaw := part.Header.Values("Content-Disposition")
		if len(dispRaw) == 0 {
			continue
		}
		disp, ok := busboyDisposition(latin1(dispRaw[0]))
		if !ok || disp.typ != "form-data" {
			continue
		}
		name, hasName := disp.params["name"]
		// busboy tests each for truthiness, so filename="" is no filename
		// and the part is a text field.
		filename, hasFile := disp.params["filename*"], true
		if filename == "" {
			filename = disp.params["filename"]
		}
		if filename == "" {
			hasFile = false
		} else {
			filename = busboyBasename(filename)
		}

		partType, charset, encoding := "text/plain", "utf8", "7bit"
		if v := part.Header.Values("Content-Type"); len(v) > 0 {
			if pct, ok := busboyContentType(latin1(v[0])); ok {
				partType = pct.typ + "/" + pct.subtype
				if cs, ok := pct.params["charset"]; ok {
					charset = strings.ToLower(cs)
				}
			}
		}
		if v := part.Header.Values("Content-Transfer-Encoding"); len(v) > 0 {
			encoding = strings.ToLower(latin1(v[0]))
		}

		if partType == "application/octet-stream" || hasFile {
			if !hasName {
				return nil, nil, &errUpload{"Field name missing"}
			}
			if filename == "" {
				// No filename, no file: multer drains the part and moves on.
				continue
			}
			files = append(files, map[string]any{
				"fieldname":    name,
				"originalname": decodeFormDataName(filename),
				"encoding":     encoding,
				"mimetype":     partType,
				"buffer":       engine.ImmutableBytes(data),
				"size":         float64(len(data)),
			})
			continue
		}

		if !hasName {
			return nil, nil, &errUpload{"Field name missing"}
		}
		if len(data) >= maxUploadFieldBytes {
			return nil, nil, &errUpload{"Field value too long"}
		}
		value, ok := busboyDecode(data, charset)
		if !ok {
			// A charset busboy cannot decode makes the value undefined, and
			// an undefined property is no property once it leaves the flow.
			continue
		}
		if err := appendField(fields, name, value); err != nil {
			return nil, nil, err
		}
	}
	return fields.export().(map[string]any), files, nil
}

// latin1 reads bytes the way busboy reads a header: one byte, one character.
// That is why a UTF-8 filename sent without filename* arrives garbled in
// Node-RED, and why it arrives garbled here.
func latin1(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		b.WriteRune(rune(s[i]))
	}
	return b.String()
}

// busboyDecode is busboy's convertToUTF8. Any charset it does not name fails:
// its fallback decoder is an arrow function, so the charset it is bound to
// never reaches TextDecoder, which throws on what it gets instead, and busboy
// swallows that and returns undefined.
func busboyDecode(data []byte, charset string) (string, bool) {
	switch strings.ToLower(charset) {
	case "utf8", "utf-8":
		return toValidUTF8(data), true
	case "latin1", "ascii", "us-ascii", "iso-8859-1", "iso8859-1", "iso88591", "iso_8859-1",
		"windows-1252", "iso_8859-1:1987", "cp1252", "x-cp1252":
		return latin1(string(data)), true
	case "utf16le", "utf-16le", "ucs2", "ucs-2":
		u := make([]uint16, len(data)/2)
		for i := range u {
			u[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
		}
		return string(utf16.Decode(u)), true
	case "base64":
		return base64.StdEncoding.EncodeToString(data), true
	}
	if len(data) == 0 {
		return "", true
	}
	return "", false
}

// toValidUTF8 replaces each byte that is not part of a UTF-8 sequence with
// U+FFFD, which is close enough to Node's decoder for bytes that were never
// text.
func toValidUTF8(p []byte) string {
	if utf8.Valid(p) {
		return string(p)
	}
	var b strings.Builder
	for len(p) > 0 {
		r, size := utf8.DecodeRune(p)
		b.WriteRune(r)
		p = p[size:]
	}
	return b.String()
}

// decodeFormDataName is multer's: only %0A, %0D and %22 come back, because
// those are the only three a browser escapes.
func decodeFormDataName(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			switch strings.ToUpper(s[i+1 : i+3]) {
			case "0A":
				b.WriteByte('\n')
				i += 2
				continue
			case "0D":
				b.WriteByte('\r')
				i += 2
				continue
			case "22":
				b.WriteByte('"')
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// busboyBasename strips a path off a filename on either kind of slash, and
// makes . and .. nothing.
func busboyBasename(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	if p == "." || p == ".." {
		return ""
	}
	return p
}

// busboyParsed is a content type or disposition, the way busboy reads one.
type busboyParsed struct {
	typ, subtype string
	params       map[string]string
}

func isToken(c byte) bool {
	return c > 0x20 && c < 0x7f && !strings.ContainsRune(`"(),/:;<=>?@[\]{}`, rune(c))
}

func isQDText(c rune) bool {
	return c == '\t' || c == ' ' || c == '!' || (c >= 0x23 && c <= 0x5b) || (c >= 0x5d && c <= 0x7e) || (c >= 0x80 && c <= 0xff)
}

func isCharsetChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		strings.IndexByte("!#$%&+-^_`{}~", c) >= 0
}

func isAttrChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' ||
		strings.IndexByte("!#$&+-.^_`|~", c) >= 0
}

// The parsers below index runes, not bytes, because busboy indexes the
// characters of a latin1 string and every byte is one.

// busboyContentType is busboy's parseContentType.
func busboyContentType(s string) (busboyParsed, bool) {
	str := []rune(s)
	if len(str) == 0 {
		return busboyParsed{}, false
	}
	i := 0
	for ; i < len(str); i++ {
		if !isTokenRune(str[i]) {
			if str[i] != '/' || i == 0 {
				return busboyParsed{}, false
			}
			break
		}
	}
	if i == len(str) {
		return busboyParsed{}, false
	}
	typ := strings.ToLower(string(str[:i]))
	i++
	subStart := i
	params := map[string]string{}
	for ; i < len(str); i++ {
		if !isTokenRune(str[i]) {
			if i == subStart {
				return busboyParsed{}, false
			}
			if !busboyParams(str, i, params, false) {
				return busboyParsed{}, false
			}
			break
		}
	}
	if i == subStart {
		return busboyParsed{}, false
	}
	return busboyParsed{typ: typ, subtype: strings.ToLower(string(str[subStart:i])), params: params}, true
}

// busboyDisposition is busboy's parseDisposition, with multer's latin1 as the
// decoder for plain values.
func busboyDisposition(s string) (busboyParsed, bool) {
	str := []rune(s)
	if len(str) == 0 {
		return busboyParsed{}, false
	}
	params := map[string]string{}
	i := 0
	for ; i < len(str); i++ {
		if !isTokenRune(str[i]) {
			if !busboyParams(str, i, params, true) {
				return busboyParsed{}, false
			}
			break
		}
	}
	return busboyParsed{typ: strings.ToLower(string(str[:i])), params: params}, true
}

func isTokenRune(r rune) bool { return r < 0x80 && isToken(byte(r)) }

// busboyParams is parseContentTypeParams, or with extended set,
// parseDispositionParams, which also takes RFC 5987 name*=charset'lang'value.
func busboyParams(str []rune, i int, params map[string]string, extended bool) bool {
	n := len(str)
	for i < n {
		for ; i < n && (str[i] == ' ' || str[i] == '\t'); i++ {
		}
		if i == n {
			break
		}
		if str[i] != ';' {
			return false
		}
		i++
		for ; i < n && (str[i] == ' ' || str[i] == '\t'); i++ {
		}
		if i == n {
			return false
		}
		nameStart := i
		for ; i < n; i++ {
			if !isTokenRune(str[i]) {
				if str[i] == '=' {
					break
				}
				return false
			}
		}
		if i == n {
			return false
		}
		name := string(str[nameStart:i])
		var value string

		if extended && strings.HasSuffix(name, "*") {
			i++
			csStart := i
			for ; i < n; i++ {
				if !(str[i] < 0x80 && isCharsetChar(byte(str[i]))) {
					if str[i] != '\'' {
						return false
					}
					break
				}
			}
			if i == n {
				return false
			}
			charset := string(str[csStart:i])
			i++
			for ; i < n && str[i] != '\''; i++ {
			}
			if i == n {
				return false
			}
			i++
			if i == n {
				return false
			}
			var raw []byte
			for ; i < n; i++ {
				c := str[i]
				if c < 0x80 && isAttrChar(byte(c)) {
					raw = append(raw, byte(c))
					continue
				}
				if c == '%' {
					if i+2 < n {
						hi, ok1 := hexVal(str[i+1])
						lo, ok2 := hexVal(str[i+2])
						if ok1 && ok2 {
							raw = append(raw, hi<<4|lo)
							i += 2
							continue
						}
					}
					return false
				}
				break
			}
			decoded, ok := busboyDecode(raw, charset)
			if !ok {
				return false
			}
			value = decoded
		} else {
			i++
			if i == n {
				return false
			}
			if str[i] == '"' {
				i++
				start := i
				escaping := false
				var b []rune
				for ; i < n; i++ {
					c := str[i]
					if c == '\\' {
						if escaping {
							start = i
							escaping = false
						} else {
							b = append(b, str[start:i]...)
							escaping = true
						}
						continue
					}
					if c == '"' {
						if escaping {
							start = i
							escaping = false
							continue
						}
						b = append(b, str[start:i]...)
						break
					}
					if escaping {
						start = i - 1
						escaping = false
					}
					if !isQDText(c) {
						return false
					}
				}
				if i == n {
					return false
				}
				i++
				value = string(b)
			} else {
				start := i
				for ; i < n; i++ {
					if !isTokenRune(str[i]) {
						if i == start {
							return false
						}
						break
					}
				}
				value = string(str[start:i])
			}
		}

		name = strings.ToLower(name)
		if _, dup := params[name]; !dup {
			params[name] = value
		}
	}
	return true
}

func hexVal(r rune) (byte, bool) {
	switch {
	case r >= '0' && r <= '9':
		return byte(r - '0'), true
	case r >= 'a' && r <= 'f':
		return byte(r-'a') + 10, true
	case r >= 'A' && r <= 'F':
		return byte(r-'A') + 10, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// append-field
// ---------------------------------------------------------------------------

// append-field builds nested objects and arrays out of field names like
// line[3][temp] and tags[]. Its objects and arrays are mutated in place as
// fields arrive, so they are pointers here until the form is done.

type afObject struct{ m map[string]any }

type afArray struct{ items []any }

// afHole is a slot JavaScript never assigned in a sparse array. It becomes
// null, which is what JSON.stringify makes of it.
type afHole struct{}

func newAFObject() *afObject { return &afObject{m: map[string]any{}} }

type afStep struct {
	isArray  bool
	key      string
	index    int
	nextType string // "", "array", "object"
	last     bool
	append   bool
}

// afParsePath is append-field's parse-path.
func afParsePath(key string) []afStep {
	failure := []afStep{{key: key, last: true}}
	first := key
	if i := strings.IndexByte(key, '['); i >= 0 {
		first = key[:i]
	}
	if first == "" {
		return failure
	}
	pos := len(first)
	steps := []afStep{{key: first}}
	tail := &steps[0]
	for pos < len(key) {
		if key[pos] == '[' && pos+1 < len(key) && key[pos+1] == ']' {
			pos += 2
			tail.append = true
			if pos != len(key) {
				return failure
			}
			continue
		}
		rest := key[pos:]
		if len(rest) > 2 && rest[0] == '[' {
			end := strings.IndexByte(rest, ']')
			if end > 1 {
				inner := rest[1:end]
				if isDigits(inner) {
					idx, err := strconv.Atoi(inner)
					if err != nil || idx > maxUploadArrayIndex {
						idx = -1
					}
					pos += end + 1
					tail.nextType = "array"
					// parseInt: on an object the key is the number, so
					// a[007] and a[7] are the same slot.
					steps = append(steps, afStep{isArray: true, index: idx, key: strconv.Itoa(idx)})
					tail = &steps[len(steps)-1]
					continue
				}
				pos += end + 1
				tail.nextType = "object"
				steps = append(steps, afStep{key: inner})
				tail = &steps[len(steps)-1]
				continue
			}
		}
		return failure
	}
	tail.last = true
	return steps
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// appendField is append-field's appendField.
func appendField(store *afObject, key, value string) error {
	steps := afParsePath(key)
	for _, s := range steps {
		if s.isArray && s.index < 0 {
			return &errUpload{"Field array index too large: " + key}
		}
	}
	var ctx any = store
	for _, s := range steps {
		next, err := afSetValue(ctx, s, afGet(ctx, s), value)
		if err != nil {
			return err
		}
		ctx = next
	}
	return nil
}

func afGet(ctx any, s afStep) any {
	switch c := ctx.(type) {
	case *afObject:
		if v, ok := c.m[s.key]; ok {
			return v
		}
	case *afArray:
		if s.isArray && s.index < len(c.items) {
			if _, hole := c.items[s.index].(afHole); !hole {
				return c.items[s.index]
			}
		}
	}
	return nil
}

func afPut(ctx any, s afStep, v any) {
	switch c := ctx.(type) {
	case *afObject:
		c.m[s.key] = v
	case *afArray:
		for len(c.items) <= s.index {
			c.items = append(c.items, afHole{})
		}
		c.items[s.index] = v
	}
}

func afSetLast(ctx any, s afStep, cur any, value string) {
	switch c := cur.(type) {
	case nil:
		if s.append {
			afPut(ctx, s, &afArray{items: []any{value}})
		} else {
			afPut(ctx, s, value)
		}
	case *afArray:
		c.items = append(c.items, value)
	case *afObject:
		afSetLast(c, afStep{key: "", last: true}, c.m[""], value)
	default:
		afPut(ctx, s, &afArray{items: []any{c, value}})
	}
}

func afSetValue(ctx any, s afStep, cur any, value string) (any, error) {
	if s.last {
		afSetLast(ctx, s, cur, value)
		return ctx, nil
	}
	switch c := cur.(type) {
	case nil:
		var next any
		if s.nextType == "array" {
			next = &afArray{}
		} else {
			next = newAFObject()
		}
		afPut(ctx, s, next)
		return next, nil
	case *afObject:
		return c, nil
	case *afArray:
		if s.nextType == "array" {
			return c, nil
		}
		obj := newAFObject()
		for i, item := range c.items {
			if _, hole := item.(afHole); !hole {
				obj.m[strconv.Itoa(i)] = item
			}
		}
		afPut(ctx, s, obj)
		return obj, nil
	default:
		obj := newAFObject()
		obj.m[""] = c
		afPut(ctx, s, obj)
		return obj, nil
	}
}

// export turns the in-progress tree into plain message values.
func (o *afObject) export() any { return afExport(o) }

func afExport(v any) any {
	switch t := v.(type) {
	case *afObject:
		out := make(map[string]any, len(t.m))
		for k, e := range t.m {
			out[k] = afExport(e)
		}
		return out
	case *afArray:
		out := make([]any, len(t.items))
		for i, e := range t.items {
			if _, hole := e.(afHole); hole {
				out[i] = nil
				continue
			}
			out[i] = afExport(e)
		}
		return out
	}
	return v
}
