import test from 'node:test';
import assert from 'node:assert/strict';
import {readFileSync} from 'node:fs';
import vm from 'node:vm';
const source=readFileSync(new URL('./dashboard.mjs',import.meta.url),'utf8');
const code=source.slice(source.indexOf('function countdown('),source.indexOf('function pct('));
function env(state){const c={state,Date,timestamp:v=>{const d=new Date(v);return Number.isFinite(+d)&&d.getFullYear()>1970?d:null;},element:()=>({dataset:{},append(){}})};vm.createContext(c);vm.runInContext(code,c);return c;}
test('countdown updates locally and elapsed times do not go negative',()=>{const c=env({});const n={dataset:{countdown:new Date(Date.now()+65000).toISOString(),prefix:'剩余 '}};c.updateCountdown(n);assert.match(n.textContent,/剩余 00:01:/);n.dataset.countdown=new Date(Date.now()-1000).toISOString();c.updateCountdown(n);assert.equal(n.textContent,'已到时间，等待调度校验');});
test('unstarted 5h exposes global admission countdown',()=>{const c=env({warmup_enabled:true,warmup_traffic:{hold_reason:'min_interval',next_allowed_at:new Date(Date.now()+60000).toISOString()}});const children=[];c.warmupWait({auth_id:'a',windows:[{window:'5h',cycle_started:false}]},{append:(...x)=>children.push(...x)});assert.equal(children.length,2);assert.match(children[1].textContent,/最早准入/);});
