// Written by CloudGround: runs one invocation of a function (#143,
// internal/functions SPEC §4). systemd hands over the accepted connection
// on file descriptor 3; one request is read, the handler called, one
// answer written, and the process exits.
import http from "node:http";
import net from "node:net";
import { pathToFileURL } from "node:url";

const started = Date.now();
const name = process.env.CG_FUNCTION || "";
const timeoutMs = Math.max(250, Number(process.env.CG_TIMEOUT_MS) || 9750);
const maxBody = 6 << 20;
let done = false;

function log(status, error) {
  const line = { time: new Date().toISOString(), status, ms: Date.now() - started };
  if (error) line.error = String(error).slice(0, 500);
  process.stdout.write(JSON.stringify(line) + "\n");
}

function send(res, status, headers, body) {
  if (done) return;
  done = true;
  const buf = Buffer.isBuffer(body) ? body : Buffer.from(body == null ? "" : String(body));
  res.writeHead(status, { ...headers, "Content-Length": buf.length, Connection: "close" });
  res.end(buf, () => setImmediate(() => process.exit(0)));
}

function answer(res, value) {
  if (value !== null && typeof value === "object" && !Buffer.isBuffer(value) && "statusCode" in value) {
    const status = Number(value.statusCode) || 200;
    if (status < 100 || status > 599) throw new Error("statusCode must be 100 to 599");
    const headers = {};
    for (const [k, v] of Object.entries(value.headers || {})) headers[k] = String(v);
    let body = value.body == null ? "" : value.body;
    if (typeof body !== "string") body = JSON.stringify(body);
    log(status);
    return send(res, status, headers, value.isBase64Encoded ? Buffer.from(body, "base64") : body);
  }
  if (typeof value === "string") {
    log(200);
    return send(res, 200, { "Content-Type": "text/plain; charset=utf-8" }, value);
  }
  log(200);
  return send(res, 200, { "Content-Type": "application/json" }, JSON.stringify(value === undefined ? null : value));
}

function fail(res, status, message, err) {
  log(status, err ? (err.message || err) : message);
  if (err && err.stack) process.stderr.write(err.stack + "\n");
  send(res, status, { "Content-Type": "text/plain; charset=utf-8" }, message + "\n");
}

const server = http.createServer({ maxHeaderSize: 64 << 10 }, (req, res) => {
  const timer = setTimeout(() => fail(res, 504, "the function took longer than its timeout"), timeoutMs);
  const chunks = [];
  let size = 0;
  req.on("data", (c) => {
    size += c.length;
    if (size > maxBody) {
      clearTimeout(timer);
      fail(res, 413, "the request body is larger than 6 MiB");
      req.destroy();
      return;
    }
    chunks.push(c);
  });
  req.on("end", async () => {
    const raw = Buffer.concat(chunks);
    const url = new URL(req.url, "http://function");
    const prefix = "/.functions/" + name;
    let path = url.pathname.startsWith(prefix) ? url.pathname.slice(prefix.length) : url.pathname;
    if (path === "") path = "/";
    const text = raw.toString("utf8");
    const utf8 = Buffer.from(text, "utf8").equals(raw);
    const headers = {};
    for (const [k, v] of Object.entries(req.headers)) headers[k] = Array.isArray(v) ? v.join(", ") : String(v);
    const src = headers["x-cloudground-source"];
    const event = {
      method: req.method, path, query: url.search.replace(/^\?/, ""), headers,
      body: utf8 ? text : raw.toString("base64"), isBase64Encoded: !utf8,
      source: src === "schedule" || src === "test" ? src : "http",
    };
    let handler;
    try {
      const mod = await import(pathToFileURL(process.env.CG_HANDLER || "index.js").href);
      handler = mod.handler || (mod.default && mod.default.handler) || mod.default;
      if (typeof handler !== "function") throw new Error("the code exports no handler function");
    } catch (err) {
      clearTimeout(timer);
      return fail(res, 500, "the function could not be loaded", err);
    }
    try {
      const value = await handler(event);
      clearTimeout(timer);
      answer(res, value);
    } catch (err) {
      clearTimeout(timer);
      fail(res, 500, "the function failed", err);
    }
  });
});
server.on("clientError", (err, socket) => {
  log(400, err.message);
  socket.end("HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n", () => process.exit(0));
});

const conn = new net.Socket({ fd: 3, readable: true, writable: true });
conn.on("close", () => { if (!done) process.exit(0); });
server.emit("connection", conn);
