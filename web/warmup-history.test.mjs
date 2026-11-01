import test from 'node:test';
import assert from 'node:assert/strict';
import {warmupRecords} from './warmup-history.mjs';

test('expired outcome remains visible after current deduplication state disappears',()=>{
  const raw={auth_id:'a',window:'5h',attempted_at:'2026-11-01T00:00:00Z',completed_at:'2026-11-01T00:00:01Z',activated_at:'2026-11-01T00:00:01Z'};
  const records=warmupRecords({warmup_history:[raw],warmups:[]});
  assert.equal(records.length,1);assert.equal(records[0].state,'confirmed');assert.equal(records[0].current,false);
});
test('weekly and five-hour sibling state describe one actual generation call',()=>{
  const base={auth_id:'a',attempted_at:'2026-11-01T00:00:00Z'};
  const input={warmup_history:[{...base,window:'weekly',completed_at:'2026-11-01T00:00:01Z'}],warmups:[{...base,window:'weekly',state:'confirmed'},{...base,window:'5h',state:'pending_confirmation'}]};
  const records=warmupRecords(input);
  assert.equal(records.length,1);assert.equal(records[0].state,'confirmed');assert.deepEqual(records[0].covered_windows,['weekly','5h']);assert.equal(records[0].current,true);
});
test('distinct attempts and credentials retain order without mutating source',()=>{
  const earlier={auth_id:'a',window:'5h',attempted_at:'2026-11-01T00:00:00Z',error:'timeout'};
  const later={auth_id:'a',window:'5h',attempted_at:'2026-11-01T02:00:00Z',blocked:true};
  const records=warmupRecords({warmup_history:[earlier,later,{...earlier,auth_id:'b'}]});
  assert.equal(records.length,3);assert.equal(records[0].state,'blocked');assert.equal(records[1].state,'failed');assert.equal(earlier.state,undefined);
});
