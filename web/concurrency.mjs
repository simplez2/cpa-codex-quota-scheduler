const count = value => Number.isFinite(value) && value >= 0 ? value : 0;
export const waitReasons = {
 serial_primary:'无会话请求等待当前凭据', sticky_credential:'会话等待绑定凭据',
 pinned_credential:'请求等待指定凭据', native_credential:'等待 CPA 选定凭据',
 pool_full:'可用凭据排队', lifecycle_unavailable:'请求生命周期未确认'
};
export function availableQuota(account, onDemand, now=Date.now()) {
 const retryAt=Date.parse(account.adq?.provider_retry_at || '');
 return account.eligible===true && !account.auth_health?.blocked &&
  (account.fresh!==false || onDemand===true) && !(Number.isFinite(retryAt)&&retryAt>now);
}
// Capacity comes from the native ledger. Predicted quota debt and plan weights
// never become concurrency counts. Aliases of one credential count only once.
export function concurrencyView(state, usableIds) {
 const gate=state.concurrency || {};
 const credentials=gate.credentials || gate.accounts || {};
 const available=new Set(usableIds ?? (state.snapshots || []).filter(a=>availableQuota(a,state.quota_probe_on_demand)).map(a=>a.auth_id));
 const seen=new Set();let capacity=0,free=0,knownActive=0,eligible=0,full=0;
 for(const [id,row] of Object.entries(credentials)) {
  const identity=row.credential_key || id;
  if(seen.has(identity))continue;
  seen.add(identity);
  const active=count(row.active),limit=count(row.limit);
  knownActive+=active;capacity+=limit;
  const aliases=Object.keys(credentials).filter(k=>(credentials[k].credential_key || k)===identity);
  if(aliases.some(k=>available.has(k))) { eligible++;free+=Math.max(0,limit-active);if(active>=limit)full++; }
 }
 const enabled=gate.enabled===true,observed=gate.lifecycle_observed===true;
 const primary=credentials[state.serial_active_auth_id];
 const serialBusy=state.scheduler_mode==='serial' && primary && count(primary.active)>=count(primary.limit);
 let title='等待调用',tone='neutral';
 if(enabled&&!observed){title='并发保护等待 CPA 生命周期信号';tone='warning';}
 else if(enabled&&count(gate.waiting)>0){title='有请求正在排队';tone='warning';}
 else if(enabled&&seen.size>0&&eligible===0){title='暂无可用凭据';tone='warning';}
 else if(enabled&&serialBusy){title='当前凭据已满载';tone='warning';}
 else if(enabled&&eligible>0&&full===eligible){title='可用凭据已满载';tone='warning';}
 else if(count(gate.active ?? knownActive)>0){title='请求正在运行';tone='good';}
 else if(!enabled){title='并发保护已关闭';tone='warning';}
 const description=state.scheduler_mode==='serial'
  ? '已有会话等待绑定凭据，新会话可重新选择可用凭据；未携带会话标识的请求等待当前凭据。'
  : state.scheduler_mode==='balanced'
  ? '新会话分配可用凭据；同一会话等待绑定凭据。空闲凭据不会接走已经绑定的会话。'
  : '当前调度模式与 CPA 选择凭据；每个凭据独立限制并发。';
 return {enabled,observed,title,tone,description,active:count(gate.active ?? knownActive),waiting:count(gate.waiting),poolWaiting:count(gate.pool_waiting),capacity:enabled&&observed?capacity:null,free:enabled&&observed?free:null,known:seen.size,eligible,full,rejected:count(gate.rejected),max:count(gate.max_per_credential ?? gate.max_per_account),wait:count(gate.wait_seconds),policy:gate.queue_policy==='fifo_per_credential'?'先到先服务 · 预热让行':'有界排队',events:[...(gate.recent_rejections || [])].reverse()};
}
