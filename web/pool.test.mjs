import test from 'node:test';
import assert from 'node:assert/strict';
import {poolWeeklyDefaults,poolWeeklyView,poolWindowView} from './pool.mjs';
import {changesBetween,fields} from './settings.mjs';

test('weekly report defaults use requested Team equivalents and editable map',()=>{
  assert.deepEqual(poolWeeklyDefaults,{team_standard:1,team_premium:5,plus:1,pro_5x:5,pro_20x:10});
  const spec=fields.find(field=>field[0]==='pool_weekly_capacity_weights');
  assert.equal(spec[3],'weights');assert.equal(spec[2],'allocation');
  const edited={...poolWeeklyDefaults,pro_20x:12};
  assert.deepEqual(changesBetween({pool_weekly_capacity_weights:poolWeeklyDefaults},{pool_weekly_capacity_weights:edited}),{pool_weekly_capacity_weights:edited});
});

test('5h excludes uncapped capacity and distinguishes missing scope from no applicable accounts',()=>{
  const none=poolWindowView({total_capacity:0,inventory_accounts:2,accounts:0,excluded_accounts:1,no_limit_reported_accounts:1,unclassified_accounts:0},'5h');
  assert.equal(none.text,'不适用');assert.equal(none.fill,0);assert.match(none.scope,/手动排除 1 个/);
  const missing=poolWindowView({total_capacity:0,inventory_accounts:2,unclassified_accounts:2},'5h');
  assert.equal(missing.text,'待确认');assert.match(missing.detail,/缺失数据不等于无限额/);
  const partial=poolWindowView({total_capacity:10,remaining_capacity:5,remaining_percent:50,complete:true,coverage_percent:100,accounts:1,unclassified_accounts:1},'5h');
  assert.equal(partial.text,'50%');assert.equal(partial.caption,'已确认有 5h 限制的账号');assert.equal(partial.quality,'范围待确认');
  assert.match(partial.scope,/范围待确认 1 个/);assert.equal(partial.total,'10 Team');
});

test('5h expired balances remain incomplete, cached data and independent settings stay explicit',()=>{
  const view=poolWindowView({total_capacity:6,remaining_capacity:5,minimum_percent:500/6,maximum_percent:100,complete:false,coverage_percent:500/6,accounts:2,unknown_accounts:1,cached_accounts:1},'5h');
  assert.match(view.detail,/1 个账号 5h 额度待确认/);assert.equal(view.quality,'额度待确认');assert.equal(view.complete,false);
  assert.equal(poolWindowView({total_capacity:1,remaining_percent:0,remaining_capacity:0,complete:true,accounts:1},'5h').text,'0%');
  const initial={pool_weekly_capacity_weights:poolWeeklyDefaults,pool_5h_capacity_weights:poolWeeklyDefaults,pool_5h_excluded_accounts:[]};
  const changes=changesBetween(initial,{...initial,pool_5h_capacity_weights:{...poolWeeklyDefaults,pro_20x:20},pool_5h_excluded_accounts:['no-limit']});
  assert.equal(changes.pool_weekly_capacity_weights,undefined);assert.equal(changes.pool_5h_capacity_weights.pro_20x,20);assert.deepEqual(changes.pool_5h_excluded_accounts,['no-limit']);
});
test('zero remaining is real zero; incomplete coverage displays a range',()=>{
  const empty=poolWeeklyView({total_capacity:5,accounts:5,remaining_capacity:0,remaining_percent:0,complete:true,known_accounts:5});
  assert.equal(empty.text,'0%');assert.equal(empty.used,'已用 100%');assert.equal(empty.fill,0);
  const partial=poolWeeklyView({total_capacity:10,accounts:2,remaining_capacity:2,minimum_percent:20,maximum_percent:70,unknown_accounts:1,complete:false});
  assert.equal(partial.text,'20% – 70%');assert.equal(partial.complete,false);assert.match(partial.detail,/1 个账号周额度待确认/);
});
test('cached weekly estimates disclose their coverage without rounding to false zero',()=>{
  const view=poolWeeklyView({total_capacity:5,accounts:5,remaining_capacity:4.97,remaining_percent:99.4,complete:true,cached_accounts:2});
  assert.equal(view.text,'99.4%');assert.match(view.detail,/2 个账号使用缓存估算/);
  assert.equal(poolWeeklyView(null).text,'—');
});
