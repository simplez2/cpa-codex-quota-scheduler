import test from 'node:test';
import assert from 'node:assert/strict';
import {normalizeBase,resourceBase,decodeStored,readPanelKey,validKey,rememberPanelKey,readRememberedPanelKey,clearPanelSession,panelSessionName,panelSessionLifetime} from './session.mjs';

const base = 'https://cpa.example/a';
const host = 'cpa.example';
const agent = 'test-browser';
const scope = encodeURIComponent(base);
const store = values => ({getItem:key=>values[key] ?? null});
const state = (apiBase,managementKey) => JSON.stringify({state:{apiBase,managementKey,rememberPassword:true}});
const encode = raw => {
  const salt = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + host + '|' + agent);
  const bytes = new TextEncoder().encode(raw);
  return 'enc::v1::' + Buffer.from(bytes.map((byte,i)=>byte ^ salt[i % salt.length])).toString('base64');
};
const read = values => readPanelKey(store(values),base,host,agent);

test('resource origin and reverse proxy prefix select the API destination',()=>{
  assert.equal(resourceBase(base + '/v0/resource/plugins/codex-quota-scheduler/open?key=ignored#ignored'),base);
  assert.equal(normalizeBase(base + '/v0/management/'),base);
  assert.throws(()=>resourceBase('https://cpa.example/other/open'));
  for (const bad of ['https://user:pass@cpa.example','https://cpa.example?redirect=evil','https://cpa.example#secret']) assert.equal(normalizeBase(bad),'');
});
test('current CPA encrypted persisted login is accepted for this base only',()=>{
  assert.equal(read({'cli-proxy-auth':encode(state(base,'fixture-key'))}),'fixture-key');
  for (const other of ['https://elsewhere.example/a','https://cpa.example/b','http://cpa.example/a']) {
    assert.equal(read({'cli-proxy-auth':encode(state(other,'other-server-key'))}),'');
  }
  assert.equal(read({'cli-proxy-auth':state('', 'unscoped-key')}),'');
});
test('scoped panel login has precedence and cannot fall back to another selection',()=>{
  const entries = {
    ['cli-proxy-auth:selection:' + scope]:encode(JSON.stringify(base)),
    ['cli-proxy-auth:scope:' + scope + ':' + scope]:encode(state(base,'selected-key')),
    'cli-proxy-auth':state(base,'legacy-key')
  };
  assert.equal(read(entries),'selected-key');
  entries['cli-proxy-auth:selection:' + scope] = JSON.stringify('https://elsewhere.example');
  assert.equal(read(entries),'');
  entries['cli-proxy-auth:selection:' + scope] = JSON.stringify(base);
  delete entries['cli-proxy-auth:scope:' + scope + ':' + scope];
  assert.equal(read(entries),'');
});
test('scoped mismatches and cleared remembered login are not reused',()=>{
  const selected = 'cli-proxy-auth:selection:' + scope;
  const key = 'cli-proxy-auth:scope:' + scope + ':' + scope;
  assert.equal(read({[selected]:JSON.stringify(base),[key]:state('https://cpa.example/b','wrong-path')}),'');
  assert.equal(read({'cli-proxy-auth':state(base,''),apiBase:JSON.stringify(base),managementKey:'stale-key'}),'');
});
test('legacy separate entries require an explicit matching API base',()=>{
  assert.equal(read({apiBase:encode(JSON.stringify(base)),managementKey:encode(JSON.stringify('legacy-fixture'))}),'legacy-fixture');
  assert.equal(read({managementKey:'orphan-key'}),'');
  assert.equal(read({apiBase:JSON.stringify('https://cpa.example/b'),managementKey:'wrong-key'}),'');
});
test('unavailable storage and malformed or unsafe keys return no credential',()=>{
  assert.equal(readPanelKey({getItem(){throw new Error('disabled');}},base,host,agent),'');
  assert.equal(decodeStored('enc::v1::%%%%',host,agent),null);
  for (const value of ['one\r\ntwo','x'.repeat(4097),null,{},'  ']) assert.equal(validKey(value),'');
});

test('remembered login survives a new panel and expires after 24 hours',()=>{
  const values={};const storage={getItem:key=>values[key]??null,setItem:(key,value)=>values[key]=value,removeItem:key=>delete values[key]};
  const now=1000000;
  assert.equal(rememberPanelKey(storage,base,host,agent,'remembered-fixture',now),true);
  assert.equal(readRememberedPanelKey(storage,base,host,agent,now+23*3600000),'remembered-fixture');
  assert.equal(values[panelSessionName(base)].includes('remembered-fixture'),false);
  assert.equal(readRememberedPanelKey(storage,base,host,agent,now+panelSessionLifetime),'');
  assert.equal(values[panelSessionName(base)],undefined);
});

test('remembered logins remain scoped and never delete CPA login entries',()=>{
  const values={'cli-proxy-auth':state(base,'parent-key')};const storage={getItem:key=>values[key]??null,setItem:(key,value)=>values[key]=value,removeItem:key=>delete values[key]};
  rememberPanelKey(storage,base,host,agent,'fixture',1000);
  assert.equal(readRememberedPanelKey(storage,'https://cpa.example/b',host,agent,1001),'');
  assert.equal(readRememberedPanelKey(storage,'http://cpa.example/a',host,agent,1001),'');
  clearPanelSession(storage,base);assert.equal(readPanelKey(storage,base,host,agent),'parent-key');
});

test('malformed, future, wrong-browser and unsafe remembered entries fail closed',()=>{
  const values={};const storage={getItem:key=>values[key]??null,setItem:(key,value)=>values[key]=value,removeItem:key=>delete values[key]};
  for(const value of [{version:1,apiBase:base,managementKey:'x',expiresAt:1000+panelSessionLifetime+1},{version:1,apiBase:'https://other.example',managementKey:'x',expiresAt:2000},{version:1,apiBase:base,managementKey:'x\ny',expiresAt:2000}]) {
    values[panelSessionName(base)]=JSON.stringify(value);assert.equal(readRememberedPanelKey(storage,base,host,agent,1000),'');
  }
  rememberPanelKey(storage,base,host,agent,'fixture',1000);
  assert.equal(readRememberedPanelKey(storage,base,host,'different-browser',1001),'');
  const unavailable={getItem(){throw new Error('blocked');},setItem(){throw new Error('blocked');}};
  assert.equal(rememberPanelKey(unavailable,base,host,agent,'fixture',1000),false);
  assert.equal(readRememberedPanelKey(unavailable,base,host,agent,1001),'');
});
