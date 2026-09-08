import test from 'node:test';
import assert from 'node:assert/strict';
import {normalizeBase,resourceBase,decodeStored,readPanelKey,validKey} from './session.mjs';

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
