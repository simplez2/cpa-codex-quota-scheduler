import test from 'node:test';
import assert from 'node:assert/strict';
import {changesBetween,conflictingFields,equal,fields,weeklyAllocationLabel} from './settings.mjs';

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

test('per-credential concurrency controls are editable in allocation and save independently',()=>{
  const settings=fields.filter(f=>f[0].startsWith('account_'));
  assert.deepEqual(settings.map(f=>f[0]),['account_concurrency_enabled','account_max_concurrency','account_concurrency_wait']);
  for(const spec of settings)assert.equal(spec[2],'allocation');
  assert.equal(settings[0][1],'限制每凭据并发');
  assert.equal(settings[1][1],'每凭据最大并发');
  assert.match(settings[1][4],/不同 CPA 凭据独立计数/);
  assert.deepEqual(changesBetween({account_concurrency_enabled:true,account_max_concurrency:2,reserve_5h_percent:0},{account_concurrency_enabled:false,account_max_concurrency:2,reserve_5h_percent:0}),{account_concurrency_enabled:false});
});

test('weekly allocation options describe both modes and the displayed policy follows the saved setting',()=>{
  const spec=fields.find(f=>f[0]==='serial_allocation_policy');
  assert.equal(spec[1],'周额度分配方式');
  assert.match(spec[4],/串行和均衡并发均生效/);
  assert.match(spec[4],/已有会话保持绑定/);
  assert.deepEqual(spec[5].map(option=>option[0]),['sustainable','weekly_remaining']);
  assert.equal(weeklyAllocationLabel('sustainable'),'按距重置时间的日均预算');
  assert.equal(weeklyAllocationLabel('weekly_remaining'),'按周剩余比例');
  assert.deepEqual(changesBetween({scheduler_mode:'balanced',serial_allocation_policy:'sustainable'},{scheduler_mode:'balanced',serial_allocation_policy:'weekly_remaining'}),{serial_allocation_policy:'weekly_remaining'});
});
