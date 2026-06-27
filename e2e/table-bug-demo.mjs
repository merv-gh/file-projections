// Playwright demo + usability test: discover the "two writers to one table, one
// wrong" bug using the Tables view and a cross-repo table trace. Records a .webm.
//
// Run:  node e2e/table-bug-demo.mjs            (assumes UI already on :7777)
//   or: BASE=http://localhost:7777 node e2e/table-bug-demo.mjs
//
// It is also an assertion-bearing usability test: it fails if the bug isn't
// discoverable (two writers not shown, or the trace doesn't reach both entrypoints).

import { chromium } from 'playwright';
import { fileURLToPath } from 'url';
import path from 'path';

const BASE = process.env.BASE || 'http://localhost:7777';
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const OUTDIR = path.join(__dirname, '..', 'demos', 'webm');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let failures = 0;
function check(cond, msg) {
  if (!cond) { failures++; console.error('  ✗ ' + msg); } else { console.log('  ✓ ' + msg); }
}

// On-screen narration overlay so the recording reads as a story.
async function say(page, text, ms = 2200) {
  await page.evaluate((t) => {
    let n = document.getElementById('__demo_narration');
    if (!n) {
      n = document.createElement('div');
      n.id = '__demo_narration';
      n.style.cssText =
        'position:fixed;left:50%;bottom:28px;transform:translateX(-50%);z-index:99999;' +
        'background:rgba(20,18,14,.92);color:#f4f0e8;padding:12px 20px;border-radius:10px;' +
        'font:15px/1.4 -apple-system,Segoe UI,Roboto,sans-serif;max-width:80vw;text-align:center;' +
        'box-shadow:0 8px 30px rgba(0,0,0,.35);transition:opacity .25s';
      document.body.appendChild(n);
    }
    n.style.opacity = '1';
    n.textContent = t;
  }, text);
  await sleep(ms);
}

async function main() {
  const browser = await chromium.launch();
  const context = await browser.newContext({
    viewport: { width: 1280, height: 800 },
    recordVideo: { dir: OUTDIR, size: { width: 1280, height: 800 } },
  });
  const page = await context.newPage();

  console.log('Mission: a reviewer suspects rows are landing in ledger_entries that');
  console.log('the payment path would skip. Find every writer and prove it.\n');

  await page.goto(BASE, { waitUntil: 'networkidle' });
  await say(page, 'Mission: bad rows in ledger_entries. Who writes there?', 2600);

  // 1. Open the Service graph tab.
  await page.click('#tgraph');
  await sleep(800);
  await say(page, 'Service graph — the whole cross-repo project, including DB tables.', 2600);

  // 2. Search to focus the table on the graph.
  await page.fill('#gsearch', 'ledger');
  await sleep(1200);
  await say(page, 'Filter nodes: there is the ledger_entries table.', 2400);

  // 3. Switch to the Tables view.
  await page.click('#gvtables');
  await page.waitForSelector('.tablecard .tsmethod', { timeout: 8000 });
  await sleep(800);
  await say(page, 'Tables view: who writes here, who reads here.', 2600);

  // Assert the bug is visible: two writers to ledger_entries.
  const writers = await page.$$eval('.tcol .tcolhd.tcw', (els) =>
    els.map((e) => e.textContent)
  );
  const writerSites = await page.$$eval('.tcol', (cols) => {
    const w = cols.find((c) => c.querySelector('.tcolhd.tcw'));
    return w ? Array.from(w.querySelectorAll('.tsmethod')).map((m) => m.textContent) : [];
  });
  console.log('\nUsability assertions:');
  check(writerSites.length >= 2, 'two or more writers shown for ledger_entries: ' + JSON.stringify(writerSites));
  check(writerSites.some((m) => /Ledger\.write/.test(m)), 'payment path writer (Ledger.write) listed');
  check(writerSites.some((m) => /Reconciliation/.test(m)), 'reconciliation writer listed (the suspect)');

  await say(page, 'Two writers! Ledger.write AND ReconciliationController.reconcile.', 3000);

  // 4. Trace the table to see both entrypoint paths.
  await page.click('[data-tracetable="ledger_entries"]');
  await sleep(1500);
  await page.waitForSelector('.answer', { timeout: 8000 });
  await say(page, 'Trace the table: every path that ends in a write to it.', 2800);

  const answersText = await page.$eval('#traceout', (e) => e.innerText);
  check(/PaymentController/.test(answersText), 'trace shows the PaymentController (payment) path');
  check(/Reconciliation/.test(answersText), 'trace shows the Reconciliation path');
  check(/assume: order.getAmount\(\).signum\(\) > 0/.test(answersText),
    'payment path is guarded by amount > 0 (the check the bug is missing)');

  await say(page,
    'Payment path guards on amount>0. Reconciliation has NO guard — that is the bug.',
    3600);

  // scroll the answers so the contrast is on screen.
  await page.evaluate(() => {
    const el = document.querySelector('#traceout');
    if (el) el.scrollTop = el.scrollHeight;
  });
  await sleep(1800);
  await say(page, 'Found it: a second, unguarded writer to the same table.', 3000);

  await sleep(800);
  await context.close(); // finalizes the video
  await browser.close();

  // Rename the recorded video to a stable name.
  const fs = await import('fs');
  const vids = fs.readdirSync(OUTDIR).filter((f) => f.endsWith('.webm'));
  vids.sort((a, b) => fs.statSync(path.join(OUTDIR, b)).mtimeMs - fs.statSync(path.join(OUTDIR, a)).mtimeMs);
  if (vids[0]) {
    const dst = path.join(OUTDIR, 'table-bug-demo.webm');
    fs.renameSync(path.join(OUTDIR, vids[0]), dst);
    console.log('\nSaved recording: ' + dst);
  }

  if (failures) { console.error('\n' + failures + ' usability assertion(s) FAILED'); process.exit(1); }
  console.log('\nAll usability assertions passed — the bug is discoverable in the UI.');
}

main().catch((e) => { console.error(e); process.exit(1); });
