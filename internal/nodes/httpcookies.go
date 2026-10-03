package nodes

import (
	"encoding/json"
	"errors"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
)

// Cookies, the way Node-RED's HTTP nodes handle them. Node-RED does none of
// this itself: it hands msg.cookies to Express's res.cookie and res.clearCookie
// on the way out, parses the Cookie header with cookie-parser on the way in,
// and feeds msg.cookies through cookie 0.7.2 into a tough-cookie jar for an
// outbound request. So this is a port of those, rule for rule, and the golden
// file in testdata is what the real libraries produce for the same input.

// jsUndefined stands for a property that is not there, which JavaScript tells
// apart from one that is null: { value: null } clears a cookie, {} sets it to
// the string "undefined".
type jsUndefined struct{}

func prop(m map[string]any, key string) any {
	if v, ok := m[key]; ok {
		return v
	}
	return jsUndefined{}
}

// jsTruthy is JavaScript's truthiness for a message value.
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil, jsUndefined:
		return false
	case bool:
		return t
	case float64:
		return t != 0 && !math.IsNaN(t)
	case int:
		return t != 0
	case string:
		return t != ""
	}
	return true
}

// jsString is String(v).
func jsString(v any) string {
	switch t := v.(type) {
	case jsUndefined:
		return "undefined"
	case nil:
		return "null"
	case time.Time:
		return t.UTC().Format("Mon Jan 02 2006 15:04:05 GMT+0000 (Coordinated Universal Time)")
	}
	return jsToString(v)
}

// jsNumber is JavaScript's unary minus-zero coercion, v - 0.
func jsNumber(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case int:
		return float64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case nil:
		return 0
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
			if n, err := strconv.ParseUint(s[2:], 16, 64); err == nil {
				return float64(n)
			}
			return math.NaN()
		}
		switch s {
		case "Infinity", "+Infinity":
			return math.Inf(1)
		case "-Infinity":
			return math.Inf(-1)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil || strings.ContainsAny(s, "_xXpP") || strings.EqualFold(s, "nan") ||
			strings.EqualFold(strings.TrimLeft(s, "+-"), "inf") || strings.EqualFold(strings.TrimLeft(s, "+-"), "infinity") {
			return math.NaN()
		}
		return f
	case []any:
		switch len(t) {
		case 0:
			return 0
		case 1:
			return jsNumber(jsString(t[0]))
		}
	case time.Time:
		return float64(t.UnixMilli())
	}
	return math.NaN()
}

// encodeURIComponent escapes everything but A-Z a-z 0-9 - _ . ! ~ * ' ( ).
func encodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' ||
			strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// decodeURIComponent reports false where JavaScript's throws: a malformed
// escape or bytes that are not UTF-8.
func decodeURIComponent(s string) (string, bool) {
	out, err := url.PathUnescape(s)
	if err != nil || !utf8.ValidString(out) {
		return s, false
	}
	return out, true
}

// cookieDecode is cookie 0.7.2's default decode: only a value with a % in it
// is decoded, and one that does not decode stays as it was.
func cookieDecode(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	out, _ := decodeURIComponent(s)
	return out
}

// cookiePair is one name and value, in the order the header had them.
type cookiePair struct{ name, value string }

// parseCookieHeader is cookie 0.7.2's parse. The first occurrence of a name
// wins, surrounding double quotes come off, and decode runs on each value.
func parseCookieHeader(str string, decode func(string) string) []cookiePair {
	var out []cookiePair
	seen := map[string]bool{}
	n := len(str)
	if n < 2 {
		return nil
	}
	index := 0
	for index < n {
		eq := strings.IndexByte(str[index:], '=')
		if eq == -1 {
			break
		}
		eq += index
		end := strings.IndexByte(str[index:], ';')
		if end == -1 {
			end = n
		} else {
			end += index
			if eq > end {
				// Backtrack on the prior semicolon.
				index = strings.LastIndexByte(str[:eq], ';') + 1
				continue
			}
		}
		ks := skipWS(str, index, eq)
		ke := trimWSBack(str, eq, ks)
		key := str[ks:ke]
		if !seen[key] {
			vs := skipWS(str, eq+1, end)
			ve := trimWSBack(str, end, vs)
			if ve-vs >= 2 && str[vs] == '"' && str[ve-1] == '"' {
				vs++
				ve--
			} else if ve-vs == 1 && str[vs] == '"' {
				// One quote is both the opening and the closing one to the
				// JavaScript, which slices it to nothing.
				vs, ve = vs+1, vs+1
			}
			seen[key] = true
			out = append(out, cookiePair{key, decode(str[vs:ve])})
		}
		index = end + 1
	}
	return out
}

func skipWS(s string, i, max int) int {
	for ; i < max; i++ {
		if s[i] != ' ' && s[i] != '\t' {
			return i
		}
	}
	return max
}

func trimWSBack(s string, i, min int) int {
	for i > min {
		i--
		if s[i] != ' ' && s[i] != '\t' {
			return i + 1
		}
	}
	return min
}

var (
	cookieNameRe   = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
	cookieValueRe  = regexp.MustCompile(`^"?[\x21\x23-\x2B\x2D-\x3A\x3C-\x5B\x5D-\x7E]*"?$`)
	cookieDomainRe = regexp.MustCompile(`(?i)^([.]?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)([.][a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)
	cookiePathRe   = regexp.MustCompile(`^[\x20-\x3A\x3D-\x7E]*$`)
)

// cookieValueOK is cookieValueRe with the backreference Go's regexp lacks: a
// leading quote needs a trailing one and the other way round.
func cookieValueOK(v string) bool {
	if !cookieValueRe.MatchString(v) {
		return false
	}
	open := strings.HasPrefix(v, `"`)
	shut := strings.HasSuffix(v, `"`) && len(v) > 1
	if v == `"` {
		return false
	}
	return open == shut
}

// serializeCookie is cookie 0.7.2's serialize. encode nil means
// encodeURIComponent; identity is what Node-RED passes as String.
func serializeCookie(name, val string, opt map[string]any, encode func(string) string) (string, error) {
	if encode == nil {
		encode = encodeURIComponent
		if opt != nil && jsTruthy(prop(opt, "encode")) {
			// A function is the only truthy encode that works, and a message
			// cannot carry one.
			return "", errors.New("option encode is invalid")
		}
	}
	if !cookieNameRe.MatchString(name) {
		return "", errors.New("argument name is invalid")
	}
	value := encode(val)
	if !cookieValueOK(value) {
		return "", errors.New("argument val is invalid")
	}
	str := name + "=" + value
	if opt == nil {
		return str, nil
	}
	if ma := prop(opt, "maxAge"); ma != nil && ma != (jsUndefined{}) {
		f := math.Floor(jsNumber(ma))
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", errors.New("option maxAge is invalid")
		}
		str += "; Max-Age=" + jsNumberString(f)
	}
	if d := prop(opt, "domain"); jsTruthy(d) {
		if !cookieDomainRe.MatchString(jsString(d)) {
			return "", errors.New("option domain is invalid")
		}
		str += "; Domain=" + jsString(d)
	}
	if p := prop(opt, "path"); jsTruthy(p) {
		if !cookiePathRe.MatchString(jsString(p)) {
			return "", errors.New("option path is invalid")
		}
		str += "; Path=" + jsString(p)
	}
	if e := prop(opt, "expires"); jsTruthy(e) {
		t, ok := e.(time.Time)
		if !ok || !validJSDate(t) {
			// Node-RED's msg.cookies carries a Date only when a Function node
			// made one. A string here is an error there too.
			return "", errors.New("option expires is invalid")
		}
		str += "; Expires=" + t.UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
	}
	if jsTruthy(prop(opt, "httpOnly")) {
		str += "; HttpOnly"
	}
	if jsTruthy(prop(opt, "secure")) {
		str += "; Secure"
	}
	if jsTruthy(prop(opt, "partitioned")) {
		str += "; Partitioned"
	}
	if p := prop(opt, "priority"); jsTruthy(p) {
		s, _ := p.(string)
		switch strings.ToLower(s) {
		case "low":
			str += "; Priority=Low"
		case "medium":
			str += "; Priority=Medium"
		case "high":
			str += "; Priority=High"
		default:
			return "", errors.New("option priority is invalid")
		}
	}
	if s := prop(opt, "sameSite"); jsTruthy(s) {
		switch t := s.(type) {
		case bool:
			str += "; SameSite=Strict"
		case string:
			switch strings.ToLower(t) {
			case "lax":
				str += "; SameSite=Lax"
			case "strict":
				str += "; SameSite=Strict"
			case "none":
				str += "; SameSite=None"
			default:
				return "", errors.New("option sameSite is invalid")
			}
		default:
			return "", errors.New("option sameSite is invalid")
		}
	}
	return str, nil
}

// validJSDate is whether a time fits a JavaScript Date, which ends 8.64e15 ms
// either side of the epoch.
func validJSDate(t time.Time) bool {
	ms := t.UnixMilli()
	return ms >= -8.64e15 && ms <= 8.64e15
}

// ---------------------------------------------------------------------------
// http response: msg.cookies to Set-Cookie
// ---------------------------------------------------------------------------

// responseCookies turns msg.cookies into Set-Cookie values the way Node-RED's
// HTTP Response node does through Express: a plain value sets a cookie, an
// object's value sets one with the object as its options, and null, or an
// object whose value is null, clears one.
//
// Names go in sorted order. JavaScript keeps an object's insertion order and a
// Go map has none, so sorted is the only order that is the same every time.
func responseCookies(v any, now time.Time) ([]string, error) {
	cookies, ok := v.(map[string]any)
	if !ok {
		return nil, nil
	}
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)

	var out []string
	for _, name := range names {
		c := cookies[name]
		obj, isObj := c.(map[string]any)
		var (
			line string
			err  error
		)
		switch {
		case c == nil:
			line, err = expressClearCookie(name, nil, now)
		case isObj && obj["value"] == nil && hasKey(obj, "value"):
			line, err = expressClearCookie(name, obj, now)
		case isObj:
			line, err = expressCookie(name, prop(obj, "value"), obj, now)
		default:
			if _, isArr := c.([]any); isArr {
				// An array is an object to typeof, with no value property.
				line, err = expressCookie(name, jsUndefined{}, map[string]any{}, now)
			} else {
				line, err = expressCookie(name, c, nil, now)
			}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

// expressCookie is Express 4.22's res.cookie.
func expressCookie(name string, value any, options map[string]any, now time.Time) (string, error) {
	opts := make(map[string]any, len(options)+2)
	for k, v := range options {
		opts[k] = v
	}
	if jsTruthy(prop(opts, "signed")) {
		return "", errors.New(`cookieParser("secret") required for signed cookies`)
	}

	var val string
	switch value.(type) {
	case map[string]any, []any, []byte, engine.ImmutableBytes, time.Time, nil:
		val = "j:" + jsJSONStringify(value)
	default:
		val = jsString(value)
	}

	if ma, ok := opts["maxAge"]; ok && ma != nil {
		if f := jsNumber(ma); !math.IsNaN(f) {
			opts["expires"] = jsDate(float64(now.UnixMilli()) + f)
			opts["maxAge"] = math.Floor(f / 1000)
		}
	}
	if p, ok := opts["path"]; !ok || p == nil {
		opts["path"] = "/"
	}
	return serializeCookie(name, val, opts, nil)
}

// expressClearCookie is Express 4.22's res.clearCookie: an empty value, an
// expiry at the epoch and the root path, any of which the options override.
func expressClearCookie(name string, options map[string]any, now time.Time) (string, error) {
	opts := map[string]any{"expires": time.UnixMilli(1).UTC(), "path": "/"}
	for k, v := range options {
		opts[k] = v
	}
	return expressCookie(name, "", opts, now)
}

// jsDate is new Date(ms): the milliseconds truncate toward zero, and a Date
// out of range is invalid, which serialize then refuses.
func jsDate(ms float64) any {
	if math.IsNaN(ms) || math.Abs(ms) > 8.64e15 {
		return "Invalid Date"
	}
	return time.UnixMilli(int64(math.Trunc(ms))).UTC()
}

// jsJSONStringify is JSON.stringify for a message value. Object keys come out
// sorted; see responseCookies for why.
func jsJSONStringify(v any) string {
	var b strings.Builder
	writeJSJSON(&b, v)
	return b.String()
}

func writeJSJSON(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil, jsUndefined:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			b.WriteString("null")
		} else {
			b.WriteString(jsNumberString(t))
		}
	case int:
		b.WriteString(strconv.Itoa(t))
	case string:
		writeJSString(b, t)
	case time.Time:
		writeJSString(b, t.UTC().Format("2006-01-02T15:04:05.000Z"))
	case []byte:
		writeJSJSON(b, bufferJSON(t))
	case engine.ImmutableBytes:
		writeJSJSON(b, bufferJSON(t))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJSJSON(b, e)
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		first := true
		for _, k := range keys {
			if _, skip := t[k].(jsUndefined); skip {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			writeJSString(b, k)
			b.WriteByte(':')
			writeJSJSON(b, t[k])
		}
		b.WriteByte('}')
	default:
		raw, err := json.Marshal(t)
		if err != nil {
			b.WriteString("null")
			return
		}
		b.Write(raw)
	}
}

// bufferJSON is what a Node.js Buffer serialises as.
func bufferJSON(p []byte) map[string]any {
	data := make([]any, len(p))
	for i, c := range p {
		data[i] = float64(c)
	}
	return map[string]any{"type": "Buffer", "data": data}
}

// writeJSString quotes a string the way JSON.stringify does: the short escapes
// for \b \f \n \r \t, \u00xx for the other control characters, and nothing
// else, so < > & and U+2028 go out as themselves.
func writeJSString(b *strings.Builder, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte(hex[r>>4])
				b.WriteByte(hex[r&15])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
}

// ---------------------------------------------------------------------------
// http in: the Cookie header to msg.req.cookies
// ---------------------------------------------------------------------------

// requestCookies is cookie-parser 1.4.7 with no secret: the header parsed with
// cookie's default decode, then any value starting j: replaced by its JSON when
// that parses to something truthy.
func requestCookies(header string) map[string]any {
	out := map[string]any{}
	if header == "" {
		return out
	}
	for _, p := range parseCookieHeader(header, cookieDecode) {
		out[p.name] = p.value
		if strings.HasPrefix(p.value, "j:") {
			var parsed any
			if err := json.Unmarshal([]byte(p.value[2:]), &parsed); err == nil && jsTruthy(parsed) {
				out[p.name] = parsed
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// http request: msg.cookies to the Cookie header, Set-Cookie to
// msg.responseCookies
// ---------------------------------------------------------------------------

// outboundCookies is what Node-RED puts in the request's cookie jar: the
// cookies from a cookie header first, in the header's order, then
// msg.cookies. Each one goes through serialize, and a value serialize refuses
// is an error, as it is there.
func outboundCookies(headerCookie string, msgCookies any) ([]cookiePair, error) {
	// A second cookie with a name already set replaces the first in place,
	// keeping its position. The Go jar does that itself, the same way
	// tough-cookie does, so the list just goes in in order.
	var jar []cookiePair
	put := func(serialized string) {
		name, value, _ := strings.Cut(serialized, "=")
		jar = append(jar, cookiePair{name, value})
	}
	identity := func(s string) string { return s }

	if headerCookie != "" {
		for _, p := range parseCookieHeader(headerCookie, identity) {
			s, err := serializeCookie(p.name, p.value, nil, identity)
			if err != nil {
				return nil, err
			}
			put(s)
		}
	}

	cookies, _ := msgCookies.(map[string]any)
	names := make([]string, 0, len(cookies))
	for name := range cookies {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		c := cookies[name]
		obj, isObj := c.(map[string]any)
		var (
			s   string
			err error
		)
		switch {
		case c == nil, isObj && obj["value"] == nil && hasKey(obj, "value"):
			// Clearing is the HTTP Response node's business; here it means
			// nothing is sent.
			continue
		case isObj && obj["encode"] == false:
			s, err = serializeCookie(name, jsString(prop(obj, "value")), nil, identity)
		case isObj:
			s, err = serializeCookie(name, jsString(prop(obj, "value")), nil, nil)
		default:
			if _, isArr := c.([]any); isArr {
				s, err = serializeCookie(name, "undefined", nil, nil)
			} else {
				s, err = serializeCookie(name, jsString(c), nil, nil)
			}
		}
		if err != nil {
			return nil, err
		}
		put(s)
	}
	return jar, nil
}

// extractResponseCookies is Node-RED's extractCookies: each Set-Cookie parsed
// with cookie's parse, the cookie's own value moved to value, and attributes
// kept under the names the server gave them. HttpOnly and Secure have no = and
// do not appear, exactly as they do not there.
func extractResponseCookies(setCookie []string) map[string]any {
	out := map[string]any{}
	for _, c := range setCookie {
		eq := strings.IndexByte(c, '=')
		if eq == -1 {
			continue
		}
		key := strings.TrimSpace(c[:eq])
		if key == "" {
			continue
		}
		parsed := map[string]any{}
		for _, p := range parseCookieHeader(c, cookieDecode) {
			parsed[p.name] = p.value
		}
		// In Node-RED's order: value is set from the key, then the key is
		// deleted. A cookie actually named "value" therefore loses its value,
		// there and here.
		if v, ok := parsed[key]; ok {
			parsed["value"] = v
		} else {
			delete(parsed, "value")
		}
		delete(parsed, key)
		out[key] = parsed
	}
	return out
}
