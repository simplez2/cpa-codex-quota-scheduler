import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source=readFileSync(new URL('./dashboard.mjs',import.meta.url),'utf8');
const code=source.slice(source.indexOf('function adqNumber('),source.indexOf('function renderADQ('));
const context={Number,String,Math}; vm.createContext(context); vm.runInContext(code,context);
test('ADQ diagnostics format capacity, percentages and countdowns',()=>{
  assert.equal(context.adqNumber(12.5),'12.5');
  assert.equal(context.adqNumber(Number.POSITIVE_INFINITY),'未知');
  assert.equal(context.adqPercent(.75),'75%');
  assert.equal(context.durationText(90061),'1天 01:01:01');
  assert.equal(context.adqProviderText('overload'),'上游过载');
  assert.equal(context.adqBottleneckText('WEEK_CONSTRAINED'),'周额度受限');
});
test('ADQ metric pairs expose width, waterline, reservations and last decision',()=>{
  const pairs=context.adqMetricPairs({enabled:true,using_fallback:false,reservations_active:2,reservation_collisions:1,calibrated_accounts:3,accounts_total:4,last_decision_auth_id:'a',last_decision_reason:'保持粘性',pool:{FullWidth:2,EffectiveWidth:2.5,M_week:1.2,M_5h:.9,M_phase:1.1,bottleneck:'RISK',current_demand_per_hour:3,forecast_demand_p95:15}});
  const map=Object.fromEntries(pairs);
  assert.equal(map['FullWidth / EffectiveWidth'],'2 / 2.5');
  assert.match(map['风险水位'],/周 1.2 · 5h 0.9 · 阶段 1.1/);
  assert.match(map.Reservation,/2 个在途 · 1 次冲突/);
  assert.match(map['最近决策'],/a · 保持粘性/);
});
