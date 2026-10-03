import test from 'node:test';
import assert from 'node:assert/strict';
import {weeklyBudgetPercentPerMinute,formatWeeklyBudgetRate} from './budget.mjs';

test('minute budget uses native precision and keeps zero distinct from unknown',()=>{
  assert.equal(weeklyBudgetPercentPerMinute({weekly_budget_known:true,weekly_budget_percent_per_minute:100/797.1166666667,weekly_budget_percent_per_day:13.1}),100/797.1166666667);
  assert.equal(weeklyBudgetPercentPerMinute({weekly_budget_known:true,weekly_budget_percent_per_minute:0}),0);
  assert.equal(formatWeeklyBudgetRate(0),'0% / 天');
  assert.equal(formatWeeklyBudgetRate(0.00805823),'11.6039% / 天');
  assert.equal(formatWeeklyBudgetRate(100/797.1166666667),'180.651% / 天');
  assert.equal(weeklyBudgetPercentPerMinute({weekly_budget_known:false,weekly_budget_percent_per_minute:1}),null);
  assert.equal(formatWeeklyBudgetRate(null),'未知');
});

test('small usable budgets never round down to zero',()=>{
  assert.equal(formatWeeklyBudgetRate(100/10080),'14.2857% / 天');
  assert.equal(formatWeeklyBudgetRate(0.01/10080),'0.00142857% / 天');
  assert.equal(formatWeeklyBudgetRate(40/60),'960% / 天');
});

test('legacy daily fields convert to minutes without losing precision',()=>{
  assert.equal(weeklyBudgetPercentPerMinute({weekly_budget_known:true,weekly_budget_percent_per_day:100*1440/30}),100/30);
  assert.equal(formatWeeklyBudgetRate(weeklyBudgetPercentPerMinute({weekly_budget_known:true,weekly_budget_percent_per_day:100/7})),'14.2857% / 天');
});

test('malformed and negative telemetry stays unknown',()=>{
  for(const value of [null,-1,NaN,Infinity,'0.5']) {
    assert.equal(weeklyBudgetPercentPerMinute({weekly_budget_known:true,weekly_budget_percent_per_minute:value,weekly_budget_percent_per_day:10}),null);
    assert.equal(formatWeeklyBudgetRate(value),'未知');
  }
  assert.equal(weeklyBudgetPercentPerMinute({weekly_budget_known:true}),null);
});
