import test from 'node:test';
import assert from 'node:assert/strict';
import {quotaPriorityText,resetCreditsText,observedResetsText,compareAccountPriorities} from './priority.mjs';

test('priority distinguishes confirmed weekly-only backup, missing 5h and a dormant window',()=>{
  assert.match(quotaPriorityText({selection_priority:{reason:'weekly_only_backup'}}),/仅周限.*后备/);
  assert.match(quotaPriorityText({selection_priority:{reason:'five_hour_unconfirmed'}}),/5h 待确认/);
  assert.match(quotaPriorityText({selection_priority:{reason:'five_hour_dormant'}}),/周期尚未启动/);
  assert.match(quotaPriorityText({selection_priority:{reason:'five_hour_reset_first',cached:true}}),/临近重置先用.*缓存估算/);
});

test('official reset credits distinguish absent, explicit zero and positive counts',()=>{
  assert.equal(resetCreditsText({reset_credits:0}),'官方可用重置次数：未返回');
  assert.equal(resetCreditsText({reset_credits_known:true,reset_credits:0}),'官方可用重置次数：0');
  assert.equal(resetCreditsText({reset_credits_known:true,reset_credits:3}),'官方可用重置次数：3');
  assert.equal(resetCreditsText({reset_credits_known:true,reset_credits:3,reset_credits_fresh:false}),'官方可用重置次数：3（缓存）');
  assert.match(resetCreditsText({reset_credits_known:true,reset_credits:-1}),/未返回/);
  assert.match(observedResetsText({cycle_generation:100}),/待确认/);
  assert.equal(observedResetsText({cycle_generation:6,observed_reset_count:5}),'当前周本地已观察 5h 重置 5 次');
});

test('account order follows backend priority groups instead of account names or local generations',()=>{
  const accounts=[{auth_id:'a-weekly',selection_priority:{rank:3}},{auth_id:'b-dormant',selection_priority:{rank:2}},{auth_id:'z-expiring',selection_priority:{rank:1}},{auth_id:'unknown'}];
  assert.deepEqual(accounts.sort(compareAccountPriorities).map(a=>a.auth_id),['z-expiring','b-dormant','a-weekly','unknown']);
});
