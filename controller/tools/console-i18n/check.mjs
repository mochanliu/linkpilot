#!/usr/bin/env node
// check.mjs -- walk every console view in English and report Chinese left over.
//
// The console's English comes from assets/i18n-en.js, and a missing entry fails
// silently: the Chinese string simply stays. This script is the only way to know
// the dictionary is complete, so run it after touching console.html.
//
//   node check.mjs --base http://127.0.0.1:8088 --token devcheck [--role tenant]
//   node check.mjs --base http://127.0.0.1:8088 --user vm-tenant --pass secret
//
// Requires a controller and a Chrome/Chromium with CDP; point CHROME at the
// binary (defaults to the Playwright headless shell if it is installed).
//
// Exit code 1 means at least one view still shows Chinese that is not on the
// intentional list below (the language switch itself is labelled 中文).

import {spawn} from 'node:child_process';
import os from 'node:os';
import fs from 'node:fs';
import path from 'node:path';

const args = Object.fromEntries(process.argv.slice(2).reduce((acc, cur, i, all) => {
  if (cur.startsWith('--')) acc.push([cur.slice(2), all[i + 1] && !all[i + 1].startsWith('--') ? all[i + 1] : 'true']);
  return acc;
}, []));

const BASE = args.base || 'http://127.0.0.1:8088';
const LANG = args.lang || 'en';
const PORT = Number(args.port || 9333);
const CHROME = args.chrome || process.env.CHROME || path.join(
  os.homedir(), 'Library/Caches/ms-playwright/chromium_headless_shell-1255',
  'chrome-headless-shell-mac-x64', 'chrome-headless-shell');
// Text that is meant to stay as-is: the language toggle is labelled in its own
// language, and <pre> command blocks are never machine-translated.
const ALLOWED = new Set(['中文']);

if (!fs.existsSync(CHROME)) {
  console.error('chrome not found at', CHROME, '- pass --chrome <path> or set CHROME');
  process.exit(2);
}

const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'sr-i18n-'));
const chrome = spawn(CHROME, ['--headless', '--disable-gpu', '--no-sandbox',
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`, 'about:blank'],
{stdio: 'ignore'});

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let ws, msgId = 0;
const pending = new Map();

async function connect() {
  for (let i = 0; i < 50; i++) {
    try {
      const v = await (await fetch(`http://127.0.0.1:${PORT}/json/version`)).json();
      ws = new WebSocket(v.webSocketDebuggerUrl);
      await new Promise((res, rej) => { ws.addEventListener('open', res); ws.addEventListener('error', rej); });
      ws.addEventListener('message', (ev) => {
        const m = JSON.parse(ev.data);
        if (m.id && pending.has(m.id)) {
          const {resolve, reject} = pending.get(m.id);
          pending.delete(m.id);
          m.error ? reject(new Error(JSON.stringify(m.error))) : resolve(m.result);
        }
      });
      return;
    } catch (e) {
      await sleep(200);
    }
  }
  throw new Error('chrome did not expose CDP on port ' + PORT);
}

function send(method, params = {}, sessionId) {
  const id = ++msgId;
  ws.send(JSON.stringify({id, method, params, sessionId}));
  return new Promise((resolve, reject) => pending.set(id, {resolve, reject}));
}

await connect();
const {targetId} = await send('Target.createTarget', {url: 'about:blank'});
const {sessionId} = await send('Target.attachToTarget', {targetId, flatten: true});
const s = (m, p) => send(m, p, sessionId);
await s('Page.enable');
await s('Runtime.enable');

async function ev(expression) {
  const r = await s('Runtime.evaluate', {expression, awaitPromise: true, returnByValue: true});
  if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
  return r.result.value;
}

const login = args.user
  ? {username: args.user, password: args.pass}
  : {token: args.token || 'devcheck'};

await s('Page.navigate', {url: `${BASE}/?lang=${LANG}`});
await sleep(900);
await ev(`fetch('/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:${JSON.stringify(JSON.stringify(login))}})`);
await s('Page.navigate', {url: `${BASE}/?lang=${LANG}`});
await sleep(1600);

const views = await ev(`Array.from(document.querySelectorAll('#nav a[data-view]')).map(a => a.getAttribute('data-view'))`);
if (!views.length) {
  console.error('no nav links: is the login correct, and does this build have a console?');
  chrome.kill();
  process.exit(2);
}

const CJK = /[\u3400-\u9fff\uf900-\ufaff]/;
const units = new Set();
for (const v of views) {
  await ev(`(() => { const a = document.querySelector('#nav a[data-view="${v}"]'); if (a) a.click(); })()`);
  await sleep(800);
  const found = await ev(`(() => {
    const skip = {SCRIPT:1, STYLE:1, PRE:1, CODE:1, TEXTAREA:1, KBD:1, SAMP:1};
    const out = [];
    const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
    let n;
    while ((n = w.nextNode())) {
      const p = n.parentNode;
      if (!p || skip[p.nodeName]) continue;
      const t = n.nodeValue.trim();
      if (t) out.push(t);
    }
    for (const el of document.querySelectorAll('[title],[placeholder],[aria-label],[alt]')) {
      for (const a of ['title','placeholder','aria-label','alt']) {
        const val = el.getAttribute && el.getAttribute(a);
        if (val && val.trim()) out.push(val.trim());
      }
    }
    return out;
  })()`);
  for (const u of found) if (CJK.test(u)) units.add(u);
  // Module tabs inside a page (基础配置 has eight) are separate render paths.
  const mods = await ev(`document.querySelectorAll('.modnav button').length`);
  for (let i = 0; i < mods; i++) {
    await ev(`(() => { const b = document.querySelectorAll('.modnav button')[${i}]; if (b) b.click(); })()`);
    await sleep(500);
    const more = await ev(`(() => {
      const skip = {SCRIPT:1, STYLE:1, PRE:1, CODE:1, TEXTAREA:1, KBD:1, SAMP:1};
      const out = [];
      const w = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
      let n;
      while ((n = w.nextNode())) {
        const p = n.parentNode;
        if (!p || skip[p.nodeName]) continue;
        const t = n.nodeValue.trim();
        if (t) out.push(t);
      }
      return out;
    })()`);
    for (const u of more) if (CJK.test(u)) units.add(u);
  }
}

const bad = [...units].filter((u) => !ALLOWED.has(u)).sort();
chrome.kill();
fs.rmSync(profile, {recursive: true, force: true});

console.log(`views: ${views.length}  untranslated units: ${bad.length}`);
for (const u of bad) console.log('  ' + u.slice(0, 120));
process.exit(bad.length ? 1 : 0);
