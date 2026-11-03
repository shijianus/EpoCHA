import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import { BROWSER_CASES } from './browser/cases.mjs';
import * as rules from './release-check.mjs';

const passing = (ids) => ids.map((id) => ({ id, required: true, status: 'PASS', executed: true, raw_exit_code: 0 }));

test('engineering and full profiles each have a complete attainable required plan', () => {
  assert.equal(typeof rules.requiredCheckIDs, 'function');
  const engineering = rules.requiredCheckIDs('engineering');
  const full = rules.requiredCheckIDs('full');
  assert.ok(engineering.includes('adminui-regressions'));
  assert.ok(engineering.includes('go-package-inventory'));
  assert.ok(!engineering.includes('stage5b'));
  for (const id of engineering) assert.ok(full.includes(id));
  assert.ok(full.includes('ocr-container'));
  for (const ids of [engineering, full]) {
    assert.equal(rules.summarizeChecks(passing(ids), ids).exit_code, 0);
    assert.equal(rules.summarizeChecks(passing(ids.slice(1)), ids).exit_code, 1);
    const blocked = passing(ids); blocked[0] = { ...blocked[0], status: 'BLOCKED', executed: false, raw_exit_code: null };
    assert.equal(rules.summarizeChecks(blocked, ids).exit_code, 3);
  }
});

test('Go inventory parses concatenated formatted objects and rejects ignored package inputs', () => {
  assert.equal(typeof rules.checkGoInventory, 'function');
  const root = resolve('source-fixture');
  const record = (name) => ({ Dir: join(root, name), ImportPath: 'example/'+name, GoFiles: ['main.go'], TestGoFiles: ['main_test.go'], EmbedFiles: ['view.html'] });
  const text = JSON.stringify(record('cmd/app'), null, 2)+'\n'+JSON.stringify(record('internal/core'), null, 2)+'\n';
  const files = ['cmd/app','internal/core'].flatMap((dir) => ['main.go','main_test.go','view.html'].map((name) => ({path:dir+'/'+name})));
  assert.equal(rules.checkGoInventory(text, {files}, root).packages, 2);
  assert.throws(() => rules.checkGoInventory(text, {files:files.slice(1)}, root), /outside Git inventory/);
  assert.throws(() => rules.checkGoInventory('', {files}, root), /empty/);
  assert.throws(() => rules.checkGoInventory(text.slice(0,-5), {files}, root), /truncated/);
});

function tapFor(cases, skipped = false) {
  return 'TAP version 13\n'+cases.map((c,i)=>`ok ${i+1} - ${c.title}${skipped&&i===0?' # SKIP environment unavailable':''}`).join('\n')+`\n1..${cases.length}\n# tests ${cases.length}\n# pass ${cases.length-(skipped?1:0)}\n# fail 0\n# cancelled 0\n# skipped ${skipped?1:0}\n# todo 0\n`;
}
test('browser manifest treats actual TAP skip as incomplete and rejects contradictory summary counts', () => {
  const file = 'adminui_browser.test.mjs';
  const cases = BROWSER_CASES.filter((c)=>c.file===file);
  const manifest = 'docs/scripts/browser/cases.mjs', suite='docs/scripts/browser/'+file;
  assert.equal(rules.checkBrowserManifest(manifest,suite,tapFor(cases,true)).status,'INCOMPLETE');
  const tap=tapFor(cases);
  assert.throws(()=>rules.checkBrowserManifest(manifest,suite,tap.replace('# pass 10','# pass 9').replace('# tests 10','# tests 9')));
  assert.throws(()=>rules.checkBrowserManifest(manifest,suite,tap+'# tests 10\n'));
});

test('browser manifest rejects a suite registered under a different file identity', (t) => {
  const dir=mkdtempSync(join(tmpdir(),'epocha-manifest-file-'));
  t.after(()=>rmSync(dir,{recursive:true,force:true}));
  const manifest=join(dir,'cases.mjs'),suite=join(dir,'one.test.mjs');
  writeFileSync(manifest,'export const BROWSER_CASES = '+JSON.stringify([{id:'one',title:'one',required:true,file:'one.test.mjs'},{id:'two',title:'two',required:true,file:'two.test.mjs'}])+';');
  writeFileSync(suite,'test(caseTitle("two"),()=>{});');
  assert.throws(()=>rules.checkBrowserManifest(manifest,suite,tapFor([{title:'one'}])),/file|suite/);
});
