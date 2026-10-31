// Interop test: two @automerge/automerge (JS) clients edit a document through the Go backend.
// Usage: BACKEND=http://127.0.0.1:7413 DOC_ID=<id> node sync-interop.mjs
// Users: alice/alice-pw (editor), carol/carol-pw (reader), see run.sh.
import * as A from "@automerge/automerge";
import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";

const BACKEND = process.env.BACKEND ?? "http://127.0.0.1:7413";
const DOC_ID = process.env.DOC_ID;
assert.ok(DOC_ID, "DOC_ID is required");

const b64 = (bytes) => Buffer.from(bytes).toString("base64");
const unb64 = (s) => new Uint8Array(Buffer.from(s, "base64"));
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// bearer sessions per user, like the Android app
const sessions = new Map();
async function bearer(user, pw) {
  if (!sessions.has(user)) {
    const res = await fetch(BACKEND + "/auth/login/", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest" },
      body: JSON.stringify({ username: user, password: pw, sessionType: "bearer", deviceName: "interop-js" }),
    });
    const body = await res.json();
    assert.equal(res.status, 200, JSON.stringify(body));
    sessions.set(user, body.token);
  }
  return "Bearer " + sessions.get(user);
}

async function rest(method, path, user, pw) {
  const res = await fetch(BACKEND + path, {
    method,
    headers: { Authorization: await bearer(user, pw), "X-Requested-With": "XMLHttpRequest" },
  });
  const text = await res.text();
  return { status: res.status, text, json: () => JSON.parse(text) };
}

async function waitUntil(what, cond, timeoutMs = 5000) {
  const end = Date.now() + timeoutMs;
  while (Date.now() < end) {
    if (await cond()) return;
    await sleep(20);
  }
  throw new Error("timed out waiting for " + what);
}

class Client {
  constructor(name, user, pw) {
    this.name = name;
    this.user = user;
    this.pw = pw;
    this.actor = randomBytes(16).toString("hex");
    this.doc = null;
    this.syncState = null;
    this.presences = new Map();
    this.errors = [];
    this.initial = null;
  }

  async connect() {
    const { json } = await rest("POST", "/auth/ws-ticket/", this.user, this.pw);
    const { ticket } = json();
    const url = BACKEND.replace(/^http/, "ws") + `/document/${DOC_ID}/ws/`;
    this.ws = new WebSocket(url);
    await new Promise((resolve, reject) => {
      this.ws.onopen = resolve;
      this.ws.onerror = reject;
    });
    this.ws.onmessage = (ev) => this.onMessage(JSON.parse(ev.data));
    this.ws.onclose = (ev) => (this.closeCode = ev.code);
    this.send({ type: "hello", documentId: DOC_ID, actorId: this.actor, ticket, client: "interop-js" });
    await waitUntil(`${this.name} initial-content`, () => this.doc !== null);
  }

  send(m) {
    this.ws.send(JSON.stringify(m));
  }

  onMessage(m) {
    switch (m.type) {
      case "initial-content":
        this.initial = m;
        this.doc = A.load(unb64(m.documentState), { actor: this.actor });
        assert.equal(this.doc.content, m.content, "documentState and content differ");
        this.syncState = A.initSyncState();
        this.flush();
        break;
      case "sync-request": {
        [this.doc, this.syncState] = A.receiveSyncMessage(this.doc, this.syncState, unb64(m.syncMessage));
        this.flush();
        break;
      }
      case "presence":
        this.presences.set(m.clientId, m);
        break;
      case "presence-snapshot":
        for (const p of m.peers) this.presences.set(p.clientId, p);
        break;
      case "presence-leave":
        this.presences.delete(m.clientId);
        break;
      case "error":
        this.errors.push(m.code);
        break;
    }
  }

  flush() {
    for (;;) {
      let msg;
      [this.syncState, msg] = A.generateSyncMessage(this.doc, this.syncState);
      if (!msg) return;
      this.send({ type: "sync-request", documentId: DOC_ID, syncMessage: b64(msg) });
    }
  }

  change(fn) {
    this.doc = A.change(this.doc, fn);
    this.flush();
  }

  close() {
    this.ws.close(1000);
  }
}

const alice = new Client("alice", "alice", "alice-pw");
const bob = new Client("bob", "bob", "bob-pw");
const carol = new Client("carol", "carol", "carol-pw");
await alice.connect();
await bob.connect();
await carol.connect();
assert.equal(alice.initial.epoch, bob.initial.epoch);
assert.equal(carol.initial.readOnly, true);
assert.equal(alice.initial.you.username, "alice");
const original = alice.doc.content;
console.log("loaded:", JSON.stringify(original));

// concurrent edits with non-ASCII text (UTF-16 surrogates in JS, code points in Go)
alice.change((d) => A.splice(d, ["content"], 0, 0, "Alice 😀 "));
bob.change((d) => A.splice(d, ["content"], d.content.length, 0, "\nBob äöü 🎉\n"));
await waitUntil("convergence", () => alice.doc.content === bob.doc.content && alice.doc.content.includes("Bob äöü") && alice.doc.content.includes("Alice 😀"));
await waitUntil("reader convergence", () => carol.doc.content === alice.doc.content);
console.log("converged:", JSON.stringify(alice.doc.content));

// the server has the same text
await waitUntil("server text", async () => (await rest("GET", `/document/${DOC_ID}/content/`, "alice", "alice-pw")).text === alice.doc.content);

// presence with Automerge cursors
const cursor = A.getCursor(alice.doc, ["content"], 6);
alice.send({ type: "presence", documentId: DOC_ID, selections: [{ anchor: cursor, head: cursor }] });
await waitUntil("presence on bob", () => [...bob.presences.values()].some((p) => p.username === "alice" && p.selections.length === 1));
const p = [...bob.presences.values()].find((p) => p.username === "alice");
assert.equal(A.getCursorPosition(bob.doc, ["content"], p.selections[0].head), 6);

// readers cannot change the document
carol.change((d) => A.splice(d, ["content"], 0, 0, "vandalism "));
await waitUntil("read-only error", () => carol.errors.includes("read-only"));
await sleep(300);
assert.ok(!alice.doc.content.includes("vandalism"), "reader change reached alice");

// actor map
const actors = (await rest("GET", "/actors/", "alice", "alice-pw")).json();
const owner = Object.fromEntries(actors.map((a) => [a.actorId, a.username]));
assert.equal(owner[alice.actor], "alice");
assert.equal(owner[bob.actor], "bob");

alice.close();
bob.close();
carol.close();
await waitUntil("bob sees alice leave", () => true);
console.log("interop OK");
process.exit(0);
