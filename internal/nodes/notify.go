package nodes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/HotLoop-io/hotloop-flow/internal/engine"
	"github.com/HotLoop-io/hotloop-flow/internal/node"
)

// Notification nodes: ntfy, Gotify, a Discord webhook and a Discord bot. They are
// the four HotLoop core sends alarms to, with the same settings (an ntfy server, a
// topic and a token or a login; a Gotify server and an application token; a whole
// webhook URL; a bot token and a channel) and the same rules:
//
//   - A credential is never in a log, a status or an error. Discord's webhook URL
//     is a credential in all of it, an ntfy topic on a public server is one too.
//   - The reason a server gives for a refusal is kept, because "forbidden" is the
//     whole fix, and what Flow itself sent is not repeated in it.
//   - A rate limit is waited out once, as long as the server says, and no longer
//     than a minute; after that it is an error on the message.
//
// They send what a message carries and pass the message on. Written against each
// service's own HTTP API, so there is no client library to carry.

func init() {
	registerNtfy()
	registerGotify()
	registerDiscordWebhook()
	registerDiscordBot()
}

const notifyMaxWait = time.Minute

// notifyClient posts a request, honors one Retry-After, and returns an error that
// carries the server's reason and none of what was sent. secrets are masked out of
// anything that goes back.
type notifyClient struct {
	http    *http.Client
	service string
	secrets []string
	sleep   func(context.Context, time.Duration) error
}

func newNotifyClient(service string, secrets ...string) *notifyClient {
	return &notifyClient{http: &http.Client{Timeout: 15 * time.Second}, service: service, secrets: secrets, sleep: notifyWait}
}

// notifyWait is how a rate limit is waited out; a test swaps it for a recorder.
var notifyWait = notifySleep

func notifySleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *notifyClient) mask(s string) string {
	for _, sec := range c.secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "***")
			if u, err := url.QueryUnescape(sec); err == nil && u != sec {
				s = strings.ReplaceAll(s, u, "***")
			}
		}
	}
	return s
}

// do sends the request built by mk (built again for a retry) and returns the body.
func (c *notifyClient) do(ctx context.Context, mk func() (*http.Request, error)) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		req, err := mk()
		if err != nil {
			return nil, fmt.Errorf("%s: %s", c.service, c.mask(err.Error()))
		}
		resp, err := c.http.Do(req.WithContext(ctx))
		if err != nil {
			// A transport error names the URL it was sent to, which can be the credential.
			var ue *url.Error
			msg := err.Error()
			if asURLError(err, &ue) {
				msg = ue.Err.Error()
			}
			return nil, fmt.Errorf("%s: %s", c.service, c.mask(msg))
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return body, nil
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			wait := retryAfter(resp.Header, body)
			if wait > 0 && wait <= notifyMaxWait {
				if err := c.sleep(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
		}
		return nil, fmt.Errorf("%s answered %s%s", c.service, resp.Status, c.reason(body))
	}
}

func asURLError(err error, target **url.Error) bool {
	ue, ok := err.(*url.Error)
	if ok {
		*target = ue
	}
	return ok
}

// reason is what a server said was wrong, short, and with every secret masked.
func (c *notifyClient) reason(body []byte) string {
	var j struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"errorDescription"`
		Message          string `json:"message"`
	}
	text := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &j) == nil {
		switch {
		case j.ErrorDescription != "":
			text = j.ErrorDescription
		case j.Error != "":
			text = j.Error
		case j.Message != "":
			text = j.Message
		}
	}
	if text == "" {
		return ""
	}
	if len(text) > 160 {
		text = text[:160]
	}
	return ": " + c.mask(text)
}

// retryAfter reads how long a server asked to be left alone, from the header or
// from Discord's body.
func retryAfter(h http.Header, body []byte) time.Duration {
	if v := h.Get("Retry-After"); v != "" {
		if s, err := strconv.ParseFloat(v, 64); err == nil && s >= 0 {
			return time.Duration(s * float64(time.Second))
		}
	}
	var j struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if json.Unmarshal(body, &j) == nil && j.RetryAfter > 0 {
		return time.Duration(j.RetryAfter * float64(time.Second))
	}
	return 0
}

// msgText is what a message says: its payload as text, or as JSON when it is not text.
func msgText(m *engine.Msg) string {
	switch v := m.Payload().(type) {
	case nil:
		return ""
	case string:
		return v
	case []byte:
		return string(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		return string(b)
	}
}

// pick is a property, or the message's own field of that name when the property is empty.
func pick(def *node.Definition, m *engine.Msg, prop, msgField string) string {
	if v := strings.TrimSpace(def.Node.PropString(prop, "")); v != "" {
		return v
	}
	if v, ok, _ := m.Get(msgField); ok && v != nil {
		return strings.TrimSpace(fmt.Sprint(v))
	}
	return ""
}

func notifyPriority(v string, def int) int {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return def
	case "min", "1":
		return 1
	case "low", "2":
		return 2
	case "default", "normal", "3":
		return 3
	case "high", "4":
		return 4
	case "max", "urgent", "5":
		return 5
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 10 {
		return n
	}
	return def
}

// standIn hands what a node would have sent to a flow test's stand-in, when there is
// one, so a test sees exactly the call and nothing leaves the process.
func standIn(svc node.Services, kind string, sent map[string]any) (handled bool, err error) {
	si := node.StandInOf(svc)
	if si == nil {
		return false, nil
	}
	_, err = si.Call(kind, sent)
	return true, err
}

func notifyStatus(out node.Emitter, err error, ok string) {
	if err != nil {
		out.Status(node.Status{Fill: "red", Shape: "dot", Text: truncate(err.Error(), 32)})
		return
	}
	out.Status(node.Status{Fill: "green", Shape: "dot", Text: ok})
}

func checkHTTPURL(raw, what string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("%s must be an http or https address", what)
	}
	return u, nil
}

// ---------------------------------------------------------------------------
// ntfy
// ---------------------------------------------------------------------------

type ntfyServer struct {
	base     string
	token    string
	user     string
	password string
}

func (s *ntfyServer) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

var topicRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func registerNtfy() {
	node.MustRegister(node.Descriptor{
		Type: "hotloop-flow-ntfy", Category: node.CategoryConfig, Color: colorStorage, Icon: "bell", IsConfig: true,
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own ntfy server, the same settings HotLoop core publishes alarms with."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropString, Label: "Server", Required: true, Placeholder: "https://ntfy.sh"},
			{Name: "username", Kind: node.PropString, Label: "Username", Help: "Instead of a token. Leave both empty for a server that lets anyone publish."},
			{Name: "token", Kind: node.PropCredential, Label: "Access token (tk_...)"},
			{Name: "password", Kind: node.PropCredential, Label: "Password"},
		},
		Help: "An ntfy server, ntfy.sh or one you run. A token is sent as a bearer token; a username and password as basic authentication.",
	}, func(def *node.Definition) (node.Node, error) {
		u, err := checkHTTPURL(def.Node.PropString("server", ""), "the server")
		if err != nil {
			return nil, err
		}
		tok, _ := def.Services.Credential("token")
		pw, _ := def.Services.Credential("password")
		return &ntfyServer{base: strings.TrimRight(u.String(), "/"), token: tok, user: def.Node.PropString("username", ""), password: pw}, nil
	})

	node.MustRegister(node.Descriptor{
		Type: "ntfy out", Category: node.CategoryNetwork, StandIn: true, Color: colorStorage, Icon: "bell",
		Inputs: 1, Outputs: 1, PaletteLabel: "ntfy", LabelProp: "name",
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own node."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server", ConfigType: "hotloop-flow-ntfy", Required: true},
			{Name: "topic", Kind: node.PropString, Label: "Topic", Help: "Empty takes msg.topic. On a public server the topic is the only secret there is, so it is kept out of every error and status."},
			{Name: "title", Kind: node.PropString, Label: "Title", Help: "Empty takes msg.title."},
			{Name: "priority", Kind: node.PropSelect, Label: "Priority", Default: "", Options: []node.Option{
				{Value: "", Label: "msg.priority, else default"}, {Value: "min", Label: "min"}, {Value: "low", Label: "low"},
				{Value: "default", Label: "default"}, {Value: "high", Label: "high"}, {Value: "max", Label: "max"}}},
			{Name: "tags", Kind: node.PropString, Label: "Tags", Help: "Comma separated. ntfy shows a tag that is an emoji name as the emoji."},
			{Name: "click", Kind: node.PropString, Label: "Click opens", Help: "An address the notification opens when it is tapped."},
		},
		Help: "Publishes msg.payload to an ntfy topic and passes the message on. The title, topic and priority come from the node, or from msg.title, msg.topic and msg.priority when the node leaves them empty.",
	}, func(def *node.Definition) (node.Node, error) {
		id := def.Node.PropString("server", "")
		if id == "" {
			return nil, fmt.Errorf("no server selected")
		}
		return &ntfyOut{def: def, cfgID: id}, nil
	})
}

type ntfyOut struct {
	def   *node.Definition
	cfgID string
}

func (n *ntfyOut) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	cfg, ok := n.def.Services.ConfigNode(n.cfgID)
	srv, isSrv := cfg.(*ntfyServer)
	if !ok || !isSrv {
		return fmt.Errorf("server config node %s is not an ntfy server", n.cfgID)
	}
	topic := pick(n.def, m, "topic", "topic")
	if !topicRe.MatchString(topic) {
		err := fmt.Errorf("the topic is empty or not letters, digits, - and _")
		notifyStatus(out, err, "")
		return err
	}
	body := msgText(m)
	if body == "" {
		body = " "
	}
	title := pick(n.def, m, "title", "title")
	prio := notifyPriority(pick(n.def, m, "priority", "priority"), 3)
	tags := strings.TrimSpace(n.def.Node.PropString("tags", ""))
	click := strings.TrimSpace(n.def.Node.PropString("click", ""))
	if ok, err := standIn(n.def.Services, "ntfy", map[string]any{"topic": topic, "title": title, "message": body, "priority": prio, "tags": tags, "click": click}); ok {
		notifyStatus(out, err, "sent")
		if err != nil {
			return err
		}
		out.Send(0, m)
		return nil
	}
	c := newNotifyClient("ntfy", topic, srv.token, srv.password)
	_, err := c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, srv.base+"/"+topic, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		if title != "" {
			req.Header.Set("Title", title)
		}
		req.Header.Set("Priority", strconv.Itoa(prio))
		if tags != "" {
			req.Header.Set("Tags", tags)
		}
		if click != "" {
			req.Header.Set("Click", click)
		}
		switch {
		case srv.token != "":
			req.Header.Set("Authorization", "Bearer "+srv.token)
		case srv.user != "":
			req.SetBasicAuth(srv.user, srv.password)
		}
		return req, nil
	})
	notifyStatus(out, err, "sent")
	if err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// Gotify
// ---------------------------------------------------------------------------

type gotifyServer struct {
	base  string
	token string
}

func (s *gotifyServer) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func registerGotify() {
	node.MustRegister(node.Descriptor{
		Type: "hotloop-flow-gotify", Category: node.CategoryConfig, Color: colorStorage, Icon: "bell", IsConfig: true,
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own Gotify server, the same settings HotLoop core publishes alarms with."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropString, Label: "Server", Required: true, Placeholder: "https://gotify.example.com"},
			{Name: "token", Kind: node.PropCredential, Label: "Application token", Help: "The token of an application made in Gotify, not a client token."},
		},
		Help: "A Gotify server and the token of the application Flow posts as.",
	}, func(def *node.Definition) (node.Node, error) {
		u, err := checkHTTPURL(def.Node.PropString("server", ""), "the server")
		if err != nil {
			return nil, err
		}
		tok, _ := def.Services.Credential("token")
		if tok == "" {
			return nil, fmt.Errorf("an application token is required")
		}
		return &gotifyServer{base: strings.TrimRight(u.String(), "/"), token: tok}, nil
	})

	node.MustRegister(node.Descriptor{
		Type: "gotify out", Category: node.CategoryNetwork, StandIn: true, Color: colorStorage, Icon: "bell",
		Inputs: 1, Outputs: 1, PaletteLabel: "gotify", LabelProp: "name",
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own node."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "server", Kind: node.PropConfigRef, Label: "Server", ConfigType: "hotloop-flow-gotify", Required: true},
			{Name: "title", Kind: node.PropString, Label: "Title", Help: "Empty takes msg.title, and then the name of the flow tab."},
			{Name: "priority", Kind: node.PropNumber, Label: "Priority", Default: 5, Help: "0 to 10. Empty takes msg.priority."},
		},
		Help: "Sends msg.payload to a Gotify server as a message and passes the message on.",
	}, func(def *node.Definition) (node.Node, error) {
		id := def.Node.PropString("server", "")
		if id == "" {
			return nil, fmt.Errorf("no server selected")
		}
		return &gotifyOut{def: def, cfgID: id}, nil
	})
}

type gotifyOut struct {
	def   *node.Definition
	cfgID string
}

func (n *gotifyOut) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	cfg, ok := n.def.Services.ConfigNode(n.cfgID)
	srv, isSrv := cfg.(*gotifyServer)
	if !ok || !isSrv {
		return fmt.Errorf("server config node %s is not a Gotify server", n.cfgID)
	}
	title := pick(n.def, m, "title", "title")
	if title == "" {
		title = node.TabLabelOf(n.def.Services)
	}
	if title == "" {
		title = "HotLoop Flow"
	}
	prio := notifyPriority(fmt.Sprint(n.def.Node.PropInt("priority", 5)), 5)
	if prio > 10 {
		prio = 10
	}
	text := msgText(m)
	if text == "" {
		text = " "
	}
	payload, _ := json.Marshal(map[string]any{"title": title, "message": text, "priority": prio})
	if ok, err := standIn(n.def.Services, "gotify", map[string]any{"title": title, "message": text, "priority": prio}); ok {
		notifyStatus(out, err, "sent")
		if err != nil {
			return err
		}
		out.Send(0, m)
		return nil
	}
	c := newNotifyClient("gotify", srv.token)
	_, err := c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, srv.base+"/message", bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Gotify-Key", srv.token)
		return req, nil
	})
	notifyStatus(out, err, "sent")
	if err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

// ---------------------------------------------------------------------------
// Discord
// ---------------------------------------------------------------------------

// Discord's limits on a message: 2,000 characters of content, 256 of an embed's
// title, 4,096 of its description. Counted in UTF-16 units, which is how Discord
// counts them.
const (
	discordContentMax = 2000
	discordTitleMax   = 256
	discordDescMax    = 4096
)

var discordSnowflake = regexp.MustCompile(`^[0-9]{15,25}$`)

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func truncateUTF16(s string, max int) string {
	if utf16Len(s) <= max {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if n+w > max-1 {
			break
		}
		b.WriteRune(r)
		n += w
	}
	return b.String() + "…"
}

var discordColors = map[string]int{"red": 0xE53935, "orange": 0xFB8C00, "yellow": 0xFDD835, "green": 0x43A047, "blue": 0x1E88E5, "grey": 0x757575}

// discordBody is the JSON for a message: the payload as content, or as an embed
// with a title and a color when the node has either.
func discordBody(title, color, text, username string) ([]byte, error) {
	msg := map[string]any{"allowed_mentions": map[string]any{"parse": []string{}}}
	if username != "" {
		msg["username"] = truncateUTF16(username, 80)
	}
	if title == "" && color == "" {
		msg["content"] = truncateUTF16(text, discordContentMax)
	} else {
		e := map[string]any{"description": truncateUTF16(text, discordDescMax)}
		if title != "" {
			e["title"] = truncateUTF16(title, discordTitleMax)
		}
		if c, ok := discordColors[strings.ToLower(color)]; ok {
			e["color"] = c
		}
		msg["embeds"] = []any{e}
	}
	return json.Marshal(msg)
}

func discordColorProp() node.Prop {
	return node.Prop{Name: "color", Kind: node.PropSelect, Label: "Embed color", Default: "", Options: []node.Option{
		{Value: "", Label: "none"}, {Value: "red", Label: "red"}, {Value: "orange", Label: "orange"}, {Value: "yellow", Label: "yellow"},
		{Value: "green", Label: "green"}, {Value: "blue", Label: "blue"}, {Value: "grey", Label: "grey"}}}
}

func registerDiscordWebhook() {
	node.MustRegister(node.Descriptor{
		Type: "discord out", Category: node.CategoryNetwork, StandIn: true, Color: colorStorage, Icon: "bell",
		Inputs: 1, Outputs: 1, PaletteLabel: "discord", LabelProp: "name",
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own node."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "url", Kind: node.PropCredential, Label: "Webhook URL", Help: "The whole URL is the credential: anyone who holds it can post to the channel. It is never shown in a status, a log or an error."},
			{Name: "username", Kind: node.PropString, Label: "Posts as", Help: "Empty keeps the name the webhook was made with."},
			{Name: "title", Kind: node.PropString, Label: "Title", Help: "A title or a color makes the message an embed. Empty takes msg.title."},
			discordColorProp(),
		},
		Help: "Posts msg.payload to a Discord channel through a webhook and passes the message on. Longer than Discord allows is cut, and nothing in the message can ping anyone.",
	}, func(def *node.Definition) (node.Node, error) {
		raw, _ := def.Services.Credential("url")
		u, err := checkHTTPURL(raw, "the webhook URL")
		if err != nil {
			return nil, err
		}
		q := u.Query()
		q.Set("wait", "true")
		u.RawQuery = q.Encode()
		return &discordWebhookOut{def: def, post: u.String(), secret: raw}, nil
	})
}

type discordWebhookOut struct {
	def    *node.Definition
	post   string
	secret string
}

func (n *discordWebhookOut) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	text := msgText(m)
	if strings.TrimSpace(text) == "" {
		text = "(empty message)"
	}
	body, err := discordBody(pick(n.def, m, "title", "title"), n.def.Node.PropString("color", ""), text, n.def.Node.PropString("username", ""))
	if err != nil {
		return err
	}
	if ok, serr := standIn(n.def.Services, "discord", map[string]any{"body": string(body)}); ok {
		notifyStatus(out, serr, "sent")
		if serr != nil {
			return serr
		}
		out.Send(0, m)
		return nil
	}
	c := newNotifyClient("discord", n.secret, n.post)
	_, err = c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, n.post, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	notifyStatus(out, err, "sent")
	if err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}

type discordBot struct {
	api   string
	token string
}

func (b *discordBot) Receive(context.Context, *engine.Msg, node.Emitter) error { return nil }

func registerDiscordBot() {
	node.MustRegister(node.Descriptor{
		Type: "hotloop-flow-discord-bot", Category: node.CategoryConfig, Color: colorStorage, Icon: "bell", IsConfig: true,
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own Discord bot, the same bot token HotLoop core's bot uses."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "token", Kind: node.PropCredential, Label: "Bot token"},
			{Name: "api", Kind: node.PropString, Label: "API address", Default: "https://discord.com/api/v10", Help: "Only changed to point at a test server."},
		},
		Help: "A Discord bot, to post into channels it has been added to with permission to send messages.",
	}, func(def *node.Definition) (node.Node, error) {
		tok, _ := def.Services.Credential("token")
		if tok == "" {
			return nil, fmt.Errorf("a bot token is required")
		}
		u, err := checkHTTPURL(def.Node.PropString("api", "https://discord.com/api/v10"), "the API address")
		if err != nil {
			return nil, err
		}
		return &discordBot{api: strings.TrimRight(u.String(), "/"), token: tok}, nil
	})

	node.MustRegister(node.Descriptor{
		Type: "discord bot out", Category: node.CategoryNetwork, StandIn: true, Color: colorStorage, Icon: "bell",
		Inputs: 1, Outputs: 1, PaletteLabel: "discord bot", LabelProp: "name",
		Compatibility: node.Compatibility{Level: node.CompatOnly, Notes: "HotLoop Flow's own node."},
		Props: []node.Prop{
			{Name: "name", Kind: node.PropString, Label: "Name"},
			{Name: "bot", Kind: node.PropConfigRef, Label: "Bot", ConfigType: "hotloop-flow-discord-bot", Required: true},
			{Name: "channel", Kind: node.PropString, Label: "Channel ID", Help: "The number from Copy Channel ID. Empty takes msg.channel."},
			{Name: "title", Kind: node.PropString, Label: "Title", Help: "A title or a color makes the message an embed. Empty takes msg.title."},
			discordColorProp(),
		},
		Help: "Posts msg.payload to a Discord channel as a bot and passes the message on.",
	}, func(def *node.Definition) (node.Node, error) {
		id := def.Node.PropString("bot", "")
		if id == "" {
			return nil, fmt.Errorf("no bot selected")
		}
		return &discordBotOut{def: def, cfgID: id}, nil
	})
}

type discordBotOut struct {
	def   *node.Definition
	cfgID string
}

func (n *discordBotOut) Receive(ctx context.Context, m *engine.Msg, out node.Emitter) error {
	cfg, ok := n.def.Services.ConfigNode(n.cfgID)
	bot, isBot := cfg.(*discordBot)
	if !ok || !isBot {
		return fmt.Errorf("bot config node %s is not a Discord bot", n.cfgID)
	}
	ch := pick(n.def, m, "channel", "channel")
	if !discordSnowflake.MatchString(ch) {
		err := fmt.Errorf("the channel is not a Discord channel ID (a number of 15 to 25 digits)")
		notifyStatus(out, err, "")
		return err
	}
	text := msgText(m)
	if strings.TrimSpace(text) == "" {
		text = "(empty message)"
	}
	body, err := discordBody(pick(n.def, m, "title", "title"), n.def.Node.PropString("color", ""), text, "")
	if err != nil {
		return err
	}
	if ok, serr := standIn(n.def.Services, "discord bot", map[string]any{"channel": ch, "body": string(body)}); ok {
		notifyStatus(out, serr, "sent")
		if serr != nil {
			return serr
		}
		out.Send(0, m)
		return nil
	}
	c := newNotifyClient("discord", bot.token)
	_, err = c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, bot.api+"/channels/"+ch+"/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bot "+bot.token)
		return req, nil
	})
	notifyStatus(out, err, "sent")
	if err != nil {
		return err
	}
	out.Send(0, m)
	return nil
}
