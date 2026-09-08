import test from 'node:test';
import assert from 'node:assert/strict';
import {changesBetween,conflictingFields,equal,fields} from './settings.mjs';

test('patch includes only edited fields and preserves zero reserve',()=>{
  const initial={reserve_5h_percent:0,serial_5h_handoff_mode:'429_only',warmup_enabled:false,priority:2000};
  assert.deepEqual(changesBetween(initial,{...initial,warmup_enabled:true}),{warmup_enabled:true});
  assert.deepEqual(changesBetween(initial,{...initial}),{});
});
test('editing account plans preserves other accounts and detects concurrent edits',()=>{
  const baseline={quota_account_plans:{first:'plus',second:'pro_20x'}};
  const changed=changesBetween(baseline,{quota_account_plans:{...baseline.quota_account_plans,first:'pro_5x'}});
  assert.equal(changed.quota_account_plans.second,'pro_20x');
  assert.deepEqual(conflictingFields(baseline,{quota_account_plans:{first:'plus',second:'team_standard'}},changed),['quota_account_plans']);
});
test('unrelated config edits do not conflict and omitted defaults stay distinct',()=>{
  assert.deepEqual(conflictingFields({},{warmup_enabled:true},{priority:2500}),[]);
  assert.deepEqual(conflictingFields({},{priority:1},{priority:2500}),['priority']);
  assert.deepEqual(conflictingFields({priority:0},{},{priority:2500}),['priority']);
  assert.ok(equal({a:1,b:{c:2,d:3}},{b:{d:3,c:2},a:1}));
});
test('every control has a distinct key and a Chinese label',()=>{
  assert.equal(new Set(fields.map(f=>f[0])).size,fields.length);
  for(const spec of fields)assert.match(spec[1],/[\u4e00-\u9fff]/);
});
