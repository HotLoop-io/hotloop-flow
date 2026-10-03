// The admin API client.
//
// Deliberately dependency-free. The dashboard's proxy enforces a CSP that blocks
// every external host, and an edge box has no internet, so anything not bundled
// simply would not load.

/**
 * A node type, as served by GET /nodes.
 *
 * This mirrors node.Descriptor in Go exactly. It is the entire contract between
 * the runtime and the editor: there is no per-node HTML anywhere, so a node type
 * that declares itself correctly here gets a working edit dialog with no
 * front-end change at all.
 */
export interface Descriptor {
  type: string;
  category: string;
  color: string;
  icon: string;
  inputs: number;
  outputs: number;
  /** Names a property that determines the output count, e.g. a Switch node's rules. */
  outputsProp?: string;
  /** Names the property used as the node's canvas label. */
  labelProp?: string;
  paletteLabel?: string;
  align?: string;
  isConfig?: boolean;
  hasButton?: boolean;
  inputLabels?: string[];
  outputLabels?: string[];
  props?: PropDef[];
  help?: string;
  compatibility: { level: string; notes?: string; unsupportedProps?: string[] };
}

export interface PropDef {
  name: string;
  label?: string;
  kind: string;
  default?: unknown;
  required?: boolean;
  placeholder?: string;
  help?: string;
  options?: { value: unknown; label: string }[];
  /** Narrows the type selector for a typedInput. */
  typedInputTypes?: string[];
  /**
   * Names the companion property holding the selected type for a typedInput.
   * Node-RED spells these inconsistently — pt, tot, vt — so it is explicit.
   */
  typeProp?: string;
  fields?: PropDef[];
  configType?: string;
  language?: string;
}

export interface NodeStat {
  nodeId: string;
  type: string;
  received: number;
  sent: number;
  errors: number;
  dropped: number;
  blocked: number;
  queueLen: number;
  queueCap: number;
  queueHigh: number;
}

export interface Settings {
  version: string;
  adminRoot: string;
  runtime: { inboxCapacity: number; overflow: string };
  auth: { enabled: boolean };
  discovery: { enabled: boolean };
  metrics: { enabled: boolean; path: string };
}

export interface RuntimeEvent {
  topic: string;
  data: Record<string, unknown>;
  at: string;
}

/** One entry in the deployment log, as GET /deployments lists it. */
export interface Deployment {
  seq: number;
  kind: 'deploy' | 'rollback' | 'baseline';
  rev: string;
  parentRev?: string;
  user?: string;
  remote?: string;
  note?: string;
  time: string;
  rollbackOf?: number;
}

/** A property that differs, from the engine's semantic diff. */
export interface DiffProp {
  path: string;
  old?: unknown;
  new?: unknown;
  hasOld: boolean;
  hasNew: boolean;
  secret?: boolean;
}

/** One node, tab, subflow or group that differs. */
export interface DiffEntry {
  id: string;
  type: string;
  name?: string;
  z?: string;
  kind: 'added' | 'removed' | 'changed' | 'moved';
  props?: DiffProp[];
  wires?: { port: number; to: string; added: boolean }[];
  layout?: DiffProp[];
}

/** A semantic diff, structured and as the text a person reads. */
export interface DiffResult {
  from: string;
  to: string;
  entries: DiffEntry[];
  summary: { added: number; removed: number; changed: number; moved: number };
  text: string;
}

export interface DeployResult {
  rev: string;
  deployment?: number;
  warnings?: string[];
  failures?: { id: string; type: string; error: string }[];
}

const TOKEN_KEY = 'hotloop-flow.token';

export class ApiError extends Error {
  constructor(readonly status: number, message: string, readonly mfaRequired = false) {
    super(message);
  }
}

/** What the phone needs to set up two-factor sign-in. */
export interface MFASetup {
  secret: string;
  uri: string;
  /** The QR code, one string per row, '1' for a dark module. */
  qr: string[];
}

export class Api {
  private token: string | null = null;

  constructor(private readonly base: string = '') {
    try {
      this.token = sessionStorage.getItem(TOKEN_KEY);
    } catch {
      // Private mode. The session simply will not survive a reload.
      this.token = null;
    }
  }

  get authenticated(): boolean {
    return this.token !== null;
  }

  /**
   * Session storage rather than local storage: a token is a bearer credential
   * for something that can run commands on a plant floor, and it should not
   * outlive the tab it was issued to.
   */
  private setToken(token: string | null): void {
    this.token = token;
    try {
      if (token) sessionStorage.setItem(TOKEN_KEY, token);
      else sessionStorage.removeItem(TOKEN_KEY);
    } catch {
      /* ignore */
    }
  }

  /**
   * Signs in. A user with two-factor sign-in on gets an ApiError with
   * mfaRequired set when the code is missing or wrong, and the login screen
   * asks for it.
   */
  async login(username: string, password: string, code = ''): Promise<void> {
    const res = await fetch(`${this.base}/auth/token`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(code ? { username, password, code } : { username, password }),
    });
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: res.statusText }));
      throw new ApiError(res.status, body.error ?? 'login failed', body.mfa === 'required');
    }
    const body = await res.json();
    this.setToken(body.access_token);
  }

  logout(): void {
    // Best effort: the token is dropped locally regardless of whether the
    // server round-trip succeeds.
    if (this.token) {
      void fetch(`${this.base}/auth/revoke`, {
        method: 'POST',
        headers: { Authorization: `Bearer ${this.token}` },
      }).catch(() => undefined);
    }
    this.setToken(null);
  }

  private async get<T>(path: string): Promise<T> {
    const res = await fetch(this.base + path, {
      headers: this.token ? { Authorization: `Bearer ${this.token}` } : {},
    });
    if (res.status === 401) {
      // Expired or revoked. Drop it so the UI falls back to the login screen
      // rather than looping on failed requests.
      this.setToken(null);
      throw new ApiError(401, 'session expired');
    }
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: res.statusText }));
      throw new ApiError(res.status, body.error ?? res.statusText);
    }
    return res.json() as Promise<T>;
  }

  settings(): Promise<Settings> {
    return this.get<Settings>('/settings');
  }

  nodes(): Promise<Descriptor[]> {
    return this.get<Descriptor[]>('/nodes');
  }

  stats(): Promise<{ nodes: NodeStat[] }> {
    return this.get<{ nodes: NodeStat[] }>('/runtime/stats');
  }

  flows(): Promise<{ rev: string; flows: unknown[]; warnings?: string[] }> {
    return this.get('/flows');
  }

  /**
   * Deploys a flow set.
   *
   * The revision travels in a header, and the payload stays a plain v1 array
   * when there is no note, which is exactly what the runtime persists and what
   * an operator can paste into a file. A note rides in the wrapped form instead
   * of a header, because a browser refuses to put anything outside Latin-1 in a
   * header and people write notes in their own language. Passing an empty rev
   * forces the write, which is the "overwrite theirs" branch of a conflict.
   */
  deploy(flows: unknown[], rev: string, note = ''): Promise<DeployResult> {
    const body = note.trim() ? { flows, note: note.trim() } : flows;
    return this.send<DeployResult>('POST', '/flows', body, { 'HotLoop-Flow-Deployment-Rev': rev });
  }

  /** Whether the signed-in user has two-factor sign-in on. */
  mfaStatus(): Promise<{ enabled: boolean }> {
    return this.get('/auth/mfa');
  }

  /** Starts setting up two-factor sign-in. Nothing changes at sign-in until it's confirmed. */
  mfaSetup(): Promise<MFASetup> {
    return this.send<MFASetup>('POST', '/auth/mfa/setup', {});
  }

  mfaConfirm(code: string): Promise<{ enabled: boolean }> {
    return this.send('POST', '/auth/mfa/confirm', { code: code.trim() });
  }

  mfaDisable(code: string): Promise<{ enabled: boolean }> {
    return this.send('POST', '/auth/mfa/disable', { code: code.trim() });
  }

  /** The deployment log, newest first. */
  deployments(limit = 50): Promise<{ deployments: Deployment[]; retain: number; current: string }> {
    return this.get(`/deployments?limit=${limit}`);
  }

  /** One deployment, with its flows. */
  deployment(seq: number): Promise<Deployment & { flows: unknown[] }> {
    return this.get(`/deployments/${seq}`);
  }

  /** What changed between two deployments, node by node. */
  deploymentDiff(from: number, to: number): Promise<DiffResult> {
    return this.get(`/deployments/${from}/diff/${to}`);
  }

  /** What deploying this document would change, against what is live. */
  pendingDiff(flows: unknown[]): Promise<DiffResult> {
    return this.send<DiffResult>('POST', '/flows/diff', flows);
  }

  /**
   * Deploys an earlier record again, as a new record, credentials included.
   * The rev is the one this editor last loaded, so a rollback racing
   * somebody's deploy gets the same 409 a deploy would.
   */
  rollback(seq: number, rev: string, note: string): Promise<DeployResult> {
    return this.send<DeployResult>('POST', `/deployments/${seq}/rollback`, { note: note.trim() },
      { 'HotLoop-Flow-Deployment-Rev': rev });
  }

  private async send<T>(method: string, path: string, body: unknown, headers: Record<string, string> = {}): Promise<T> {
    const res = await fetch(this.base + path, {
      method,
      headers: {
        'Content-Type': 'application/json',
        ...headers,
        ...(this.token ? { Authorization: `Bearer ${this.token}` } : {}),
      },
      body: JSON.stringify(body),
    });
    if (res.status === 401) {
      this.setToken(null);
      throw new ApiError(401, 'session expired');
    }
    if (!res.ok) {
      const err = await res.json().catch(() => ({ error: res.statusText }));
      throw new ApiError(res.status, err.error ?? res.statusText);
    }
    return res.json() as Promise<T>;
  }

  /**
   * Opens the event stream.
   *
   * A browser can't set an Authorization header on a WebSocket handshake. The
   * token used to go in the query string, which is exactly where access logs,
   * proxies and browser history keep things, so it now rides as a second
   * subprotocol beside hotloop-flow, a header nothing logs by habit. The server
   * picks hotloop-flow, so the token doesn't come back in the answer either.
   */
  connectEvents(onEvent: (e: RuntimeEvent) => void, onState: (up: boolean) => void): () => void {
    let socket: WebSocket | null = null;
    let closed = false;
    let retry = 1000;
    let timer: number | undefined;

    const open = () => {
      if (closed) return;
      const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
      // No token means the runtime was started with authentication off (the
      // editor only gets this far without one when /settings answered), and
      // the socket needs none.
      const protocols = this.token ? ['hotloop-flow', `hotloop-flow.bearer.${this.token}`] : ['hotloop-flow'];
      const url = `${proto}//${location.host}${this.base}/comms`;

      socket = new WebSocket(url, protocols);

      socket.onopen = () => {
        retry = 1000;
        onState(true);
      };

      socket.onmessage = (ev) => {
        try {
          // The server batches up to 64 events per frame, so this is always an
          // array — one frame per event would lock the browser under load.
          const batch = JSON.parse(ev.data as string) as RuntimeEvent[];
          for (const e of batch) onEvent(e);
        } catch {
          /* a malformed frame must not kill the stream */
        }
      };

      socket.onclose = () => {
        onState(false);
        if (closed) return;
        // Capped exponential backoff. An edge link drops constantly and a tight
        // reconnect loop would hammer the runtime it is trying to observe.
        timer = window.setTimeout(open, retry);
        retry = Math.min(retry * 2, 30000);
      };

      socket.onerror = () => socket?.close();
    };

    open();

    return () => {
      closed = true;
      if (timer) window.clearTimeout(timer);
      socket?.close();
    };
  }
}
