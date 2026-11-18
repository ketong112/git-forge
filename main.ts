// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind an ephemeral port, tell the env
// the port it bound, then poll until the env says the trial is over and exit 0.
//
// This entry is a thin wrapper: the forge itself is the vendored static Go core
// (./forge-core, built from go/; RULES.txt allows a statically linked Linux amd64 binary as
// vendored source). main.ts does no serving — it only:
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial container). Without it
//      there is nothing to talk to, so the program exits 1 at once.
//   2. Copy ./forge-core to /tmp (the repo is mounted read-only at /bot and a web upload lands
//      the binary without the executable bit, so it cannot run in place) and chmod it +x.
//   3. Spawn it. The core binds an ephemeral port of its own and prints "PORT <n>" as its first
//      stdout line. Never bind 8080 — the env owns it.
//   4. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address the request came
//      from plus N as the base URL and dials it from then on. The first register wins; a 409 means
//      another entry already registered, and this process keeps waiting like any other.
//   5. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env answers at once
//      ({"done": false} while the suite runs), so this is a poll, not a long-held request. A failed
//      or slow poll is retried with a short exponential backoff; the process never exits before the
//      env says done.
//   6. Kill the core and exit 0. The harness then stops the env, which writes the export the
//      scorer reads.

import { spawn, type ChildProcess } from 'node:child_process';
import { chmodSync, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;
/** The longest the core may take to print its bound port. */
const CORE_PORT_TIMEOUT_MS = 10_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer; 409 means a first register
 * already won, which is not this process's to fix. 400 and 405 are contract errors and end the program;
 * a network error or another status is retried, since the env may still be starting.
 */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

/**
 * Copy the vendored core to /tmp (executable) and spawn it. The core prints "PORT <n>" as its
 * first stdout line once it has bound its port; this resolves with that port.
 */
function spawnCore(): Promise<{ child: ChildProcess; port: number }> {
  const src = fileURLToPath(new URL('./forge-core', import.meta.url));
  const dst = join(tmpdir(), `forge-core-${process.pid}`);
  copyFileSync(src, dst);
  chmodSync(dst, 0o755);
  const child = spawn(dst, [], { stdio: ['ignore', 'pipe', 'inherit'] });
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      child.kill('SIGKILL');
      reject(new Error('the core did not print its port in time'));
    }, CORE_PORT_TIMEOUT_MS);
    let buffer = '';
    const onData = (chunk: Buffer): void => {
      buffer += chunk.toString('utf8');
      const newline = buffer.indexOf('\n');
      if (newline < 0) return;
      const line = buffer.slice(0, newline).trim();
      const match = /^PORT (\d+)$/.exec(line);
      clearTimeout(timer);
      child.stdout?.off('data', onData);
      if (match === null) {
        child.kill('SIGKILL');
        reject(new Error(`the core's first line was not a port: ${JSON.stringify(line)}`));
        return;
      }
      resolve({ child, port: Number(match[1]) });
    };
    child.stdout?.on('data', onData);
    child.once('error', (err) => {
      clearTimeout(timer);
      reject(err);
    });
    child.once('exit', (code) => {
      clearTimeout(timer);
      reject(new Error(`the core exited before printing its port (code ${String(code)})`));
    });
  });
}

/** Spawn the core, register its port, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const { child, port } = await spawnCore();
  process.stdout.write(`[entry] core listening on port ${port}\n`);
  // If the core dies mid-trial the env will never say done; fail at once
  // instead of polling /done forever (a crash would otherwise score 0
  // silently at the wall instead of surfacing the real error).
  const coreDied = new Promise<never>((_, reject) => {
    child.once('exit', (code, signal) => {
      reject(new Error(`the core exited (code ${String(code)}, signal ${String(signal)}) before the trial finished`));
    });
  });
  try {
    await Promise.race([register(port), coreDied]);
    process.stdout.write('[entry] registered; waiting for the env to say done\n');
    await Promise.race([waitForDone(), coreDied]);
  } finally {
    child.kill('SIGTERM');
  }
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
