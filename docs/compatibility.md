# Node compatibility

Generated from the node registry. Do not edit by hand — change the
`Compatibility` field on the node's descriptor and regenerate:

```
HOTLOOP_FLOW_UPDATE_DOCS=1 go test ./internal/nodes/
```

A node that is partially compatible and silent about how is worse than one
that is obviously absent: the flow appears to work and quietly does the wrong
thing. Every entry below has to say what is missing, and a test fails the
build if one does not.

## What the levels mean

| Level | Meaning |
|---|---|
| **full** | Behaves as the Node-RED node of the same type does. |
| **partial** | A subset. The notes say exactly which parts are missing. |
| **divergent** | Deliberately behaves differently. The notes say why. |
| **hotloop-flow-only** | No Node-RED counterpart. |

## Not supported at all

**Node-RED community nodes.** They are npm packages that need Node.js.
There is no version of this where they work.

## JSONata

Every property typed `jsonata` is evaluated, by gnata, a pure Go JSONata 2.x,
with Node-RED's own functions bound in: `$flowContext`, `$globalContext`, `$env`,
`$clone`, and the legacy form that names `msg`. Against the published jsonata-js
2.2.2 test suite it passes 1673 of 1686 cases. The 13 it misses are listed in
`internal/jsonata/suite_test.go`, and ten of those still raise an error, just
with a different code or token than jsonata-js gives.

Three differences from Node-RED. `$moment` is refused with an error naming
`$fromMillis` and `$toMillis`, because moment.js isn't reimplemented here. A
message object has no key order, so `$keys()` and anything that walks an object
sees its keys sorted. And an evaluation still running after 10 seconds is
stopped with D1012 instead of stalling the node's whole queue.

## Summary

53 node types registered.

| Level | Count |
|---|---|
| full | 12 |
| partial | 21 |
| divergent | 14 |
| hotloop-flow-only | 6 |

## Common

| Type | Level | Notes |
|---|---|---|
| `catch` | full | — |
| `comment` | full | — |
| `complete` | full | Watches only the nodes explicitly selected in its scope, as Node-RED does. |
| `debug` | full | — |
| `inject` | partial | Interval, startup and cron-scheduled injection are supported. The crontab is read the way Node-RED's own scheduler, cronosjs, reads it, in the process's local time, including what it does with the hour that goes missing or repeats when the clocks change. A manual inject from the admin API always sends the configured properties: Node-RED's inject-with-these-values (msg.__user_inject_props__) is not implemented. |
| `junction` | full | — |
| `link call` | full | Calls a Link In and sends on whatever a Link Out in return mode sends back, including calls made from inside a call. A static target is chosen by id. In dynamic mode msg.target is an id or a name, looked up on the calling flow first and then across every flow, never inside a subflow instance. A call with no return within the timeout raises an error with the original message; a return that turns up after that still goes out, as it does in Node-RED. |
| `link in` | full | — |
| `link out` | divergent | Both modes are supported: send to Link In nodes, and return to the Link Call that sent the message. One deliberate difference: a Link In that is not running, and a return with no Link Call to go back to, raise an error a Catch node can see. Node-RED drops the first quietly and only logs a warning for the second, which makes a deleted or mistyped link very hard to find. |
| `status` | full | — |

## Config

| Type | Level | Notes |
|---|---|---|
| `hotloop-flow-influxdb` | hotloop-flow-only | HotLoop Flow's own InfluxDB connection, targeting the App Store's influxdb-app. |
| `hotloop-flow-postgres` | hotloop-flow-only | HotLoop Flow's own PostgreSQL connection. Targets the App Store's postgresql-app and timescale-db-pod, which share a wire protocol. |
| `mqtt-broker` | partial | MQTT 3.1, 3.1.1 and 5. Connection, credentials, TLS, clean session, keepalive, and birth, close and will messages with their QoS, retain flag and, on version 5, their properties and the will delay. On version 5 also the session expiry interval and connect user properties. The receive maximum, maximum packet size and topic alias maximum settings are not implemented, and the broker's defaults apply. TLS is Node-RED's usetls with a tls-config for the CA, client certificate and server name; a broker saved by an earlier HotLoop Flow with tls: true still connects over TLS with the system roots. Ignored properties: `receiveMaximum`, `maximumPacketSize`, `topicAliasMaximum`. |
| `tls-config` | divergent | Certificate, key and CA from files, from environment variables, from uploaded PEM text, or from a PKCS#12 bundle, with a passphrase for an encrypted key or bundle, server name, ALPN protocol and the verify switch. Two differences. A path is read through the secret scope, the data directory plus secrets.allowedPaths, because a TLS config that can read any path is a way to probe the filesystem from the editor. And a config that is broken, a certificate without its key or a file that is not there, fails to deploy. Node-RED logs it and carries on, so the connection quietly goes out without the client certificate you configured. |
| `websocket-client` | partial | Connects out and reconnects on its own when the connection drops, in payload mode or whole-message mode. Per-node TLS configuration is not implemented; the system trust store is used. Ignored properties: `tls`. |
| `websocket-listener` | partial | Serves a websocket path, in payload mode or whole-message mode. The path shares the flow route table with the HTTP In nodes, so it cannot shadow the editor or the admin API and cannot collide with another node's path. A client that stops reading is disconnected rather than queued without limit, which Node-RED does not do. |

## Discover

| Type | Level | Notes |
|---|---|---|
| `netinfo` | hotloop-flow-only | HotLoop Flow's own node. Reports the interfaces the runtime can see, which in macvlan mode is how a flow learns its address on the OT VLAN. |
| `scan` | hotloop-flow-only | HotLoop Flow's own node. Sweeps a CIDR range for OT devices and identifies Modbus and EtherNet/IP endpoints. Bounded by the discovery allowlist in the runtime configuration, not by this dialog. |

## Function

| Type | Level | Notes |
|---|---|---|
| `change` | partial | set, change, delete and move are supported for msg, flow and global targets, including JSONata-typed values. The deep copy option is ignored: a value set from another property is shared with it, as it is in Node-RED with the option off. Ignored properties: `dc`. |
| `delay` | divergent | All six modes are implemented — fixed, variable, random, rate limit, per-topic queue and timed release — along with msg.reset, msg.flush and the second output for dropped messages. Two deliberate differences: the queue is bounded, and past the limit a message is refused to a Catch node rather than held, because Node-RED's unbounded queue turns a source faster than the drain into an OOM-kill with no explanation; and messages still held when the flow stops are released rather than discarded. |
| `exec` | divergent | The three outputs, both buffered and streaming modes, the timeout and the appended message property all behave as Node-RED's do. Two things do not, and neither is negotiable. There is no shell: the command line is split on quoting rules only, and an unquoted shell metacharacter is refused rather than run, so one allowed command cannot become an arbitrary one. And the node is disabled until an operator names the commands a flow may run. Node-RED's exec node runs anything, and Node-RED starts with no authentication by default; a vendor shipping that default became CVE-2025-41656 (Pilz IndustrialPI 4, VDE-2025-045), unauthenticated command execution on the device. Output is capped per stream; a command that exceeds it is killed and reported rather than being allowed to fill the heap. A command that forks children of its own may leave them behind when it is killed. |
| `function` | partial | Runs on goja, a JavaScript interpreter written in Go, rather than Node's vm module. The language is ES2023; the Node standard library is not present. require() and npm modules do not work and cannot be made to without embedding Node. There is always a CPU time limit, which Node-RED leaves optional and off. setTimeout and setInterval are not available — use a Delay or Trigger node, which the runtime can account for. Ignored properties: `libs`, `setTimeout`, `setInterval`, `require`. |
| `range` | full | — |
| `rbe` | partial | Block-unless-changed and deadband modes are supported. Narrowband modes are not implemented in this build. |
| `switch` | partial | Every comparison rule is supported, including a JSONata expression rule, which sees $I and $N for a message that is part of a sequence. The sequence rules head, tail and index are not implemented, and a rule using one fails every message with an unknown operator error. Ignored properties: `head`, `tail`, `index`. |
| `template` | partial | Mustache templating is implemented against mustache.js's dialect, including its HTML escape set and standalone-line handling, so a template moved from Node-RED renders the same bytes. Partials ({{>name}}) and custom delimiters are refused rather than ignored, because there is nothing in a flow file that can supply either. |
| `trigger` | divergent | Both messages, extend-on-retrigger, wait-to-be-reset, msg.reset, the msg.delay override, per-topic grouping and the second output are implemented. The divergence is the same as the Delay node's: the number of simultaneously armed timers is bounded, and a message past the limit is refused to a Catch node rather than silently arming another. Timers with a deadline that are still armed when the flow stops fire immediately rather than being discarded; a timer waiting to be reset is dropped, because firing it would invent an event that never happened. |

## Network

| Type | Level | Notes |
|---|---|---|
| `http in` | divergent | Serves a path, with Express-style :params and a trailing *, and builds the same msg.req / msg.res / msg.payload shape Node-RED does, including JSON, form-encoded and raw bodies. Three deliberate differences. A request that no HTTP Response node answers is closed with 504 after a timeout instead of being held open forever, because Node-RED's version leaks a connection per request until the process runs out of sockets and stops answering with nothing in the log. Two nodes claiming the same method and path is refused at deploy time rather than one of them silently never firing. And a path that would shadow the editor or the admin API is refused for the same reason. Uploads parse the way multer does in Node-RED: text fields into msg.payload, files into msg.req.files, a broken upload answered with 500. One bound multer lacks: an array index over 10,000 in a field name, a[99999999], is refused instead of building a sparse array that long. msg.req.cookies is cookie-parser's, j: values included. Ignored properties: `swaggerDoc`. |
| `http request` | partial | Method, URL, headers, basic authentication, redirects and the three return types, with msg.url, msg.method and msg.headers overriding the node. The response body is size-capped and the call is bounded by a timeout, neither of which Node-RED does. TLS comes from a tls-config, or without one msg.rejectUnauthorized = false skips the certificate check for that one message, as in Node-RED. msg.cookies and a cookie header go out through a cookie jar, cookies a redirect sets follow the redirect, and msg.responseCookies and msg.redirectList come back, all as in Node-RED. A multipart/form-data content type with an object payload sends it as a form. Proxy settings and connection persistence are not implemented in this build. There is no egress allowlist: this node can reach anything the pod can, exactly as Node-RED's can, and the place to bound that is a NetworkPolicy rather than an edit dialog nobody outside the cluster can trust. Ignored properties: `proxy`, `persist`. |
| `http response` | partial | Status code and headers from the node or from msg.statusCode and msg.headers, with the payload as the body. msg.cookies sets and clears cookies exactly as Express does for Node-RED, options and all; with more than one, they go out in name order rather than the order the object was built in. |
| `mqtt in` | partial | Topic subscription with QoS and payload decoding, and on MQTT 5 the no local, retain as published and retain handling options, with the message's properties on msg.userProperties, msg.contentType, msg.responseTopic, msg.correlationData and msg.messageExpiryInterval, and its content type steering the auto-detect decoding the way Node-RED's does. Dynamic subscription via a control message is not implemented. |
| `mqtt out` | partial | Publishing with topic, QoS and retain from the node or the message, and on MQTT 5 the content type, response topic, correlation data, message expiry and user properties, from the node or from msg.contentType, msg.responseTopic, msg.correlationData, msg.messageExpiryInterval and msg.userProperties, with msg.responseTopic as the topic when there is no other. Topic aliases are not used: the full topic always goes. The connect and disconnect control messages are not implemented. Ignored properties: `topicAlias`. |
| `tcp in` | divergent | Listens or connects out, in stream mode with a delimiter or single mode collecting until the peer closes, with buffer, string or base64 payloads. Every message carries msg._session so a TCP Out node can reply on the same connection. Three bounds Node-RED does not have: the number of accepted connections, the size of one delimited message, and the total of a single-mode read. A peer that opens connections and never closes them, or sends without ever sending the delimiter, grows the heap until the pod dies otherwise. TLS through a tls-config works both ways: a listener presents the config's certificate, a client checks the server's. |
| `tcp out` | partial | Connects to a host and sends, or replies on the connection a TCP In node accepted, found through msg._session. Listening for inbound connections purely to write to them, which Node-RED's third mode does, is not implemented — use a TCP In node for the listening half and this node in reply mode. TLS through a tls-config applies to client mode; a reply goes back down whatever the TCP In node accepted, TLS or not. |
| `tcp request` | partial | Connects, sends the payload and waits for the reply, in any of Node-RED's four wait modes: a fixed time, a delimiter, a byte count, or until the peer closes. msg.host and msg.port override the node. Every request opens its own connection — Node-RED's connection-reuse mode is not implemented, and reusing one would change the semantics of the wait modes, which all end at a connection boundary. With a tls-config the server's certificate is checked unless the config says not to. Node-RED's TCP Request never checks it, whatever the config says, which makes its TLS a padlock painted on the door. |
| `udp in` | partial | Receives datagrams, optionally joining a multicast group, with buffer, string or base64 payloads and the sender's address on msg.ip and msg.port. IPv6 and per-node interface selection are not implemented in this build. Ignored properties: `ipv6`. |
| `udp out` | partial | Sends datagrams to a host, a broadcast address or a multicast group, with msg.ip and msg.port overriding the node. IPv6 is not implemented in this build. Ignored properties: `ipv6`. |
| `websocket in` | full | Emits a message per frame, carrying msg._session so a WebSocket Out node can reply to the connection it came from. |
| `websocket out` | divergent | Replies to the connection named by msg._session, or broadcasts to every open connection when there is none, as Node-RED does. The divergence is what happens when a connection cannot keep up: the frame is refused to a Catch node and the connection is closed, rather than queued without limit. Blocking instead would push back-pressure from a slow browser into the flow's scheduler, which is a worse failure than losing a connection that had already stopped reading. |

## Parser

| Type | Level | Notes |
|---|---|---|
| `csv` | partial | Parsing to objects and rendering from objects are supported, with configurable separator and header handling. Multi-line quoted fields spanning separate messages are not reassembled. |
| `html` | partial | Extracts elements by CSS selector, returning inner HTML, text or attributes, as one message per match or one message holding an array. The selector engine covers type, id, class, attribute, descendant, child and comma groups. Pseudo-classes, pseudo-elements, sibling combinators and the ~= and \|= attribute operators are refused at deploy time rather than ignored, because a selector that quietly drops its :nth-child matches the wrong elements and keeps working. Returned HTML is re-rendered from the parse tree, so it is normalised markup rather than the original bytes. |
| `json` | partial | Conversion in both directions is supported. Schema validation against msg.schema is not implemented in this build. Ignored properties: `schema`. |
| `xml` | partial | Both directions, using xml2js's object convention: attributes under the key named by "attr" (default $), element text under "chr" (default _), and every child element as an array, which is xml2js's own explicitArray default. Set ew_explicitArray to false to collapse single children instead. The per-message msg.options that Node-RED passes through to xml2js is not honoured — the other xml2js options change the shape of the output, and silently ignoring one would produce an object the flow does not expect while looking like it worked. Namespaces are kept as part of the element name rather than being resolved. Ignored properties: `options`. |
| `yaml` | full | Conversion in both directions, toggling on the value's type when no action is set, as Node-RED's does. |

## Sequence

| Type | Level | Notes |
|---|---|---|
| `batch` | divergent | Group by count with overlap and with the end of an incoming sequence honoured, group by time interval with or without empty sequences, and concatenate sequences by topic, with msg.reset, as Node-RED does. Two configurations Node-RED accepts are refused: a count below one, which it quietly reads as one, and an interval of zero, which holds every message forever. |
| `join` | divergent | Automatic, manual and reduce modes as Node-RED has them: placement by msg.parts.index, the separator Split recorded, nested sequences, arrays, strings, buffers, objects keyed by a message property, merged objects, accumulate, msg.complete, msg.reset, msg.restartTimeout, the timeout, and a reduce expression with $A, $I and $N and an optional fixup. One deliberate difference: an automatic join given a message without msg.parts raises an error rather than logging a warning. |
| `sort` | full | Sorts an array property by its elements or by a JSONata key evaluated against each element, and a message sequence by a property or a JSONata key evaluated against each message. |
| `split` | divergent | Splits arrays, objects, strings and buffers, by delimiter, by a byte sequence or by length, including streaming mode, which carries an unfinished piece over to the next message. msg.parts matches Node-RED's, separator and nested sequences included. Three deliberate differences: an object is split in sorted key order, because Go maps have none; a string is split by length in characters rather than UTF-16 code units, so no character is cut in half; and a payload that cannot be split raises an error instead of vanishing. |

## Storage

| Type | Level | Notes |
|---|---|---|
| `file` | divergent | Append, overwrite and delete, with the filename from a literal, a message property, context or the environment, and utf8, base64, hex or raw encodings. The divergence is the path scope: the file nodes may only reach the data directory and whatever else the operator listed, resolved through symlinks so a link planted on the PVC cannot point out of it. Node-RED's file nodes take any path, which makes editing a flow equivalent to reading any file the process can. Writes are fsynced by default, which Node-RED's are not. |
| `file in` | divergent | Whole-file, per-line and chunked reads, with utf8, base64, hex or raw output. Same path scope as the File node, and for the same reason. A read is also size-capped: Node-RED reads a whole file into memory with no limit, so pointing the node at the wrong path is an OOM-kill rather than an error. Past the cap the node says which limit was hit and that per-line or chunked mode would work. |
| `influxdb out` | hotloop-flow-only | HotLoop Flow's own node. The type name matches the community node-red-contrib-influxdb so an imported flow finds it, but the configuration is not identical — check the fields after importing. |
| `postgres` | hotloop-flow-only | HotLoop Flow's own node. Writes to and reads from PostgreSQL or TimescaleDB, with batch insert for message sequences. |
| `watch` | divergent | Reports files and directories appearing, changing and being removed, with the same message shape Node-RED produces. It polls rather than using the kernel's notification interface, so a change is seen within the poll interval rather than immediately, and two changes inside one interval are reported once. That is a deliberate trade: fsnotify means per-platform code and a filename suffix that is a build constraint, which has already cost this codebase a day. Same path scope as the other file nodes, and the number of watched entries is capped so a recursive watch on a large tree cannot stall the runtime. |

