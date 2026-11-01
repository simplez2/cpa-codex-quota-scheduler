function validTime(value) {
  const time=Date.parse(value||'');
  return Number.isFinite(time)&&time>0 ? time : 0;
}

export function warmupRecords(state={}) {
  const records=new Map();
  for(const [current,source] of [[false,state.warmup_history||[]],[true,state.warmups||[]]]) {
    for(const raw of source) {
      const key=raw.auth_id+'|'+validTime(raw.attempted_at);
      const previous=records.get(key);
      const covered=[...new Set([...(previous?.covered_windows||[]),...(raw.covered_windows||[]),raw.window].filter(Boolean))];
      const state=raw.state||(raw.blocked?'blocked':raw.error?'failed':validTime(raw.activated_at)?'confirmed':validTime(raw.completed_at)?'pending_confirmation':'attempted');
      // A sibling window from the same call may still await confirmation.
      // Preserve confirmed evidence while combining covered-window labels.
      const record=previous?.state==='confirmed'&&state==='pending_confirmation'?previous:{...raw,state};
      records.set(key,{...record,covered_windows:covered,current:current||previous?.current||false});
    }
  }
  return [...records.values()].sort((a,b)=>validTime(b.outcome_at||b.attempted_at)-validTime(a.outcome_at||a.attempted_at));
}
