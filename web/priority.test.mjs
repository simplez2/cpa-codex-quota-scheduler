import test from 'node:test';
import assert from 'node:assert/strict';
import {quotaPriorityText,resetCreditsText,resetCreditDetails,resetCreditView,observedResetsText,compareAccountPriorities} from './priority.mjs';

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

test('reset credit details use supported cards instead of the zero usage applicability hint',()=>{
  const d={applicable_known:true,applicable:0,applicable_fresh:true,supported_available_known:true,supported_available_count:2,supported_available_fresh:true,count:2,complete:true,list_fresh:true,auto_enabled:false,credits:[
    {expires_at:'2026-11-07T00:04:39.734115Z'},{expires_at:'2026-11-30T20:42:04.744230Z'}
  ]};
  const text=resetCreditDetails({reset_credit_details:d},Date.parse('2026-11-01T00:00:00Z')).join('\n');
  assert.match(text,/当前套餐可用重置卡：2 张/);
  assert.doesNotMatch(text,/当前允许使用|官方当前不允许/);
  assert.match(text,/2026\/11\/07 08:04:39（GMT\+8）/);
  assert.match(text,/2026\/12\/01 04:42:04（GMT\+8）/);
  assert.match(text,/自动用卡：已关闭/);
  assert.doesNotMatch(text,/已过期/);
  assert.match(resetCreditDetails({reset_credit_details:{...d,auto_enabled:true,auto_before:'10m0s',auto_reason:'previous_attempt_requires_review'}}).join('\n'),/停止自动消耗/);
});

test('raw usage applicability alone does not establish card availability',()=>{
  const raw=resetCreditDetails({reset_credit_details:{applicable_known:true,applicable:0,applicable_fresh:true,auto_enabled:false}}).join('\n');
  assert.doesNotMatch(raw,/当前允许使用|当前套餐可用重置卡/);
  const cached=resetCreditDetails({reset_credit_details:{supported_available_known:true,supported_available_count:2,supported_available_fresh:false,auto_enabled:false}}).join('\n');
  assert.match(cached,/2 张（缓存，使用前重新核验）/);
});

test('missing and stale reset data never imply zero, unlimited credits or verified success',()=>{
  assert.deepEqual(resetCreditDetails({}),[]);
  const lines=resetCreditDetails({reset_credit_details:{count:2,complete:false,list_fresh:false,error:'reset_credits_http_503',auto_enabled:true,auto_reason:'details_incomplete',last_action:'unknown',action_at:'2026-11-01T00:00:00Z',credits:[{expires_at:'bad'},{expires_at:'2026-10-31T00:00:00Z'}]}},Date.parse('2026-11-01T00:00:00Z')).join('\n');
  assert.match(lines,/查询失败/);
  assert.match(lines,/已过期/);
  assert.match(lines,/不自动重试/);
  assert.doesNotMatch(lines,/当前允许使用：0|已核验|reset_credits_http_503|Invalid Date/);
});

test('compact reset card view keeps unknown, zero and independent counts distinct',()=>{
  const unknown=resetCreditView({reset_credits:0,reset_credit_details:{applicable_known:true,applicable:0}});
  assert.equal(unknown.official,null);
  assert.equal(unknown.supported,null);
  const account={reset_credits_known:true,reset_credits:3,reset_credits_fresh:false,reset_credit_details:{supported_available_known:true,supported_available_count:0,supported_available_fresh:false,auto_enabled:false}};
  const view=resetCreditView(account);
  assert.equal(view.official,3);
  assert.equal(view.supported,0);
  assert.equal(view.officialCached,true);
  assert.equal(view.supportedCached,true);
  assert.equal(view.automatic,false);
  assert.equal(resetCreditView({}).automatic,null);
});

test('compact reset view highlights earliest future expiry without hiding expired or unsupported cards',()=>{
  const account={reset_credit_details:{list_fresh:true,auto_enabled:true,auto_before:'10m0s',auto_reason:'scheduled',credits:[
    {expires_at:'2026-12-01T20:42:04Z',supported:true},
    {expires_at:'2026-11-07T00:04:39Z',supported:false},
    {expires_at:'2026-10-31T00:00:00Z',supported:true},
    {expires_at:'bad',supported:true},
  ]}};
  const before=JSON.stringify(account);
  const view=resetCreditView(account,Date.parse('2026-11-03T00:00:00Z'));
  assert.equal(view.nextExpiry,'2026-11-07T00:04:39Z');
  assert.equal(view.nextExpiryText,'2026/11/07 08:04');
  assert.equal(view.credits.length,3);
  assert.equal(view.credits[1].supported,false);
  assert.equal(view.credits[2].expired,true);
  assert.equal(view.autoBefore,'10 分钟');
  assert.equal(view.automaticText,'等待临近到期');
  assert.equal(JSON.stringify(account),before);
});

test('compact reset warnings remain explicit when an automatic attempt needs review',()=>{
  const view=resetCreditView({reset_credit_details:{count:2,complete:false,list_fresh:false,auto_enabled:true,auto_reason:'previous_attempt_requires_review',last_action:'unknown',action_at:'2026-11-01T00:00:00Z'}});
  assert.match(view.automaticText,/停止自动消耗/);
  assert.match(view.notes.join('\n'),/自动用卡暂停/);
  assert.match(view.notes.join('\n'),/结果待核查，不自动重试/);
  assert.doesNotMatch(view.notes.join('\n'),/已核验/);
});
