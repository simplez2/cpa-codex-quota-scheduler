const plans = [['team_standard','Team Standard · 1×'],['plus','Plus · 1×'],['pro_5x','Pro 5x · 5×'],['team_premium','Team Premium · 5×'],['pro_20x','Pro 20x · 20×']];
const handoff = [['threshold_only','达到使用阈值'],['reserve_aware','保留安全余量']];
// Metadata is presentation only. Defaults and validation come from the running
// plugin so an omitted YAML field is never silently saved as zero or empty.
export const fields = [
  ['scheduler_mode','调度模式','allocation','select','均衡并发按会话分配：新会话按余量、周预算及套餐容量选择账号；同一会话的续聊、工具调用和并发请求保持绑定。账号不可用时才切换。', [['balanced','均衡并发（会话粘性）'],['serial','串行调配'],['legacy','传统调度'],['shadow','观察对比'],['enforce','动态节奏控制']]],
  ['serial_allocation_policy','串行模式的周额度分配方式','allocation','select','均衡并发始终按周日均预算分配。', [['sustainable','按距重置时间的日均预算'],['weekly_remaining','按周剩余比例']]],
  ['quota_default_plan','无法识别时的默认套餐','allocation','select','优先自动识别 CPA 返回的套餐；只有标签缺失、未知或缓存过期时使用此默认值。倍率是容量参考。',plans],
  ['serial_budget_rebalance_percent','日均预算优势达到多少时换号','allocation','number','百分比；0 关闭主动再平衡。仍需两次独立额度确认。',0,100,1],
  ['serial_weekly_rebalance_min_hold','主动换号前至少持有','allocation','duration','1 分钟至 24 小时。额度耗尽或 429 不受此等待限制。'],
  ['reserve_weekly_percent','周额度保留比例','allocation','number','百分比；仅在所有账号都进入保留区时才继续使用保留区。',0,99.9,.1],
  ['serial_5h_handoff_mode','5h 换号时机','allocation','select','“额度用尽再切换”不使用静态、预测或缓存年龄预留。',[['429_only','额度用尽 / 上游限额时切换（零预留）'],['custom_threshold','达到指定使用比例'],['reserve_aware','保留指定余量'],['inherit_global','沿用通用阈值']]],
  ['serial_5h_switch_percent','5h 已用比例阈值','allocation','number','仅“达到指定使用比例”模式生效。',.1,100,.1],
  ['reserve_5h_percent','5h 预留比例','allocation','number','零预留模式忽略此项；当前选择不会偷偷扣除余量。',0,99.9,.1],
  ['serial_prefer_active_cycle','优先使用已开始的周期','allocation','boolean','在符合额度条件的账号中优先考虑已开始计时的周期。'],
  ['serial_soft_continuation','允许旧会话越过软阈值','allocation','boolean','关闭时，后续会话请求跟随新账号；已输出内容的请求不回放。'],
  ['quota_account_plans','每个账号的套餐','allocation','plans','默认自动识别 Team Standard、Team Premium 和 Plus。手动覆盖优先于自动识别，仅影响调度参考，不会更改订阅。'],
  ['warmup_enabled','自动预热','warmup','boolean','开启后产生少量真实模型请求；已确认周期不重复预热。均衡模式在真实请求进行中及结束后的短暂间隔内暂缓预热。'],
  ['warmup_model','预热模型','warmup','text','填写当前 CPA 支持的模型名称；不会更改客户端的默认模型。'],
  ['warmup_min_interval','两次预热至少间隔','warmup','duration','全账号池共用，1 分钟至 24 小时。'],
  ['warmup_max_per_day','滚动 24 小时最多预热','warmup','number','全账号池共用，失败也计入次数。',1,1000,1],
  ['warmup_retry_after','失败重试的基础等待','warmup','duration','至少 1 分钟；连续失败会延长等待，达到限制后需手动恢复。'],
  ['refresh_interval','活跃账号查询基础间隔','advanced','duration','默认 30 秒。均衡模式余量充足时降至每分钟；收到新额度响应头时延后重复查询，接近耗尽时加快。'],
  ['quota_refresh_cooldown','备用账号查询间隔','advanced','duration','30 秒至 24 小时；查询错误会独立退避。'],
  ['quota_refresh_batch','每轮最多查询账号数','advanced','number','请求按顺序发送；默认 8。',1,100,1],
  ['stale_after','快照有效期','advanced','duration','必须不短于当前账号查询间隔。'],
  ['fallback_ban','429 未提供重置时间时冷却','advanced','duration','无法确定上游重置时间时使用。'],
  ['max_ban','自动冷却最长时间','advanced','duration','必须不短于上述默认冷却。'],
  ['half_open_probe_timeout','恢复探测最长占用','advanced','duration','1 分钟至 2 小时；同一账号只允许一个恢复探测。'],
  ['half_open_retry_after','恢复探测失败后等待','advanced','duration','至少 1 秒且不超过默认冷却。'],
  ['cpa_management_url','CPA 内部管理请求地址','connection','url','插件从服务器内部访问此地址，通常以 /v0/management/api-call 结尾。'],
  ['cpa_management_key_file','管理密钥文件路径','connection','text','服务器或容器中的已挂载文件路径；面板不会读取或显示密钥内容。'],
  ['state_path','调度状态文件路径','connection','text','保存账号选择、额度缓存、冷却及预热记录。更换路径前需迁移已有状态。'],
  ['quota_url','上游额度查询地址','connection','url','默认使用 ChatGPT 原生额度接口。'],
  ['priority','调度插件优先级','expert','number','多个调度插件并存时使用，较大的优先。',-100000,100000,1],
  ['serial_switch_percent','通用已用比例阈值','expert','number','适用于通用阈值策略。',.1,100,.1],
  ['serial_handoff_mode','通用换号方式','expert','select','不覆盖单独设置的 5h 换号方式。',handoff],
  ['serial_weekly_rebalance_percent','按周剩余比例换号的优势','expert','number','百分点；仅按周剩余比例的分配方式使用。0 关闭。',0,100,1],
  ['drain_window_hours','临近重置的消耗窗口','expert','number','小时；更倾向在重置前使用剩余额度。',.1,168,.1],
  ['switch_hysteresis_percent','切换比较的余量带宽','expert','number','百分点；避免余量相近时频繁切换。',0,100,.1],
  ['prefer_reset_credits','同类候选优先有重置次数的账号','expert','boolean','仅在其他额度条件允许时比较。'],
  ['window_order','额度窗口比较顺序','expert','select','同类额度窗口的优先顺序。', [['5h,weekly,monthly','5h → 周 → 月'],['5h,monthly,weekly','5h → 月 → 周'],['weekly,5h,monthly','周 → 5h → 月'],['weekly,monthly,5h','周 → 月 → 5h'],['monthly,5h,weekly','月 → 5h → 周'],['monthly,weekly,5h','月 → 周 → 5h']]],
  ['reserve_monthly_percent','月额度保留比例','expert','number','存在月额度窗口时使用。',0,99.9,.1],
  ['soft_limit_percent','动态调度软阈值','expert','number','已用百分比；主要用于非串行模式。',.1,100,.1],
  ['low_quota_percent','进入低余量区的比例','expert','number','剩余百分比；提升动态消耗估计分位。',.1,100,.1],
  ['sticky_seconds','会话绑定空闲有效期','allocation','number','秒；默认 1500（25 分钟）。请求和完成时续期，生成中的会话不按空闲过期；0 关闭绑定。均衡模式不因其他账号额度更多而打断绑定。',0,864000,1],
  ['switch_confirmations','动态候选连续获胜次数','expert','number','主要用于非串行会话绑定切换。',1,100,1],
  ['cost_sample_limit','保留的请求成本样本数','expert','number','动态节奏评估的内存样本上限。',32,100000,1],
  ['decision_history_limit','保留的调度决策条数','expert','number','只保留脱敏后的决策记录。',1,10000,1],
  ['normal_cost_quantile','正常请求成本分位','expert','number','0 至 1，且正常 ≤ 低余量 ≤ 高成本。',.01,1,.01],
  ['guard_cost_quantile','低余量请求成本分位','expert','number','主要用于动态节奏评估。',.01,1,.01],
  ['high_cost_quantile','高成本请求成本分位','expert','number','主要用于动态节奏评估。',.01,1,.01],
  ['shadow_log_interval','观察模式日志间隔','expert','duration','0 秒关闭汇总日志。']
];

export function equal(a,b) {
  const stable = value => value && typeof value === 'object' ? Array.isArray(value) ? value.map(stable) : Object.fromEntries(Object.keys(value).sort().map(k=>[k,stable(value[k])])) : value;
  return JSON.stringify(stable(a)) === JSON.stringify(stable(b));
}
export function changesBetween(initial,draft) { return Object.fromEntries(Object.keys(draft).filter(k=>!equal(initial[k],draft[k])).map(k=>[k,draft[k]])); }
export function conflictingFields(baseline,current,changes) { return Object.keys(changes).filter(k=>!equal(baseline[k],current[k])); }

const make=(tag,cls,text)=>{const n=document.createElement(tag);if(cls)n.className=cls;if(text!==undefined)n.textContent=text;return n;};
const byId=id=>document.getElementById(id);
export function createSettingsEditor({api,notify,onSaved,onView}) {
  let initial={},draft={},raw={},loaded=false,saving=false,serial=0,accounts=[],loading=null;
  const controls=new Map();
  const form=byId('settings-form');
  function changed() {return changesBetween(initial,draft);}
  function dirty(){return loaded && Object.keys(changed()).length>0;}
  function update() {
    form.inert=saving;
    const count=Object.keys(changed()).length;
    byId('save-bar').hidden=!count && !saving;
    byId('save-summary').textContent=saving?'正在写入 CPA 并核对生效状态…':count+' 项未保存';
    byId('save-settings').disabled=saving;
    byId('discard-settings').disabled=saving;
    const mode=draft.serial_5h_handoff_mode;
    for(const name of ['serial_allocation_policy','serial_budget_rebalance_percent','serial_weekly_rebalance_min_hold','serial_prefer_active_cycle','serial_soft_continuation']) {
      const row=controls.get(name)?.closest('.setting-field');if(row)row.hidden=draft.scheduler_mode==='balanced';
    }
    for(const name of ['serial_5h_switch_percent','reserve_5h_percent']) {
      const row=controls.get(name)?.closest('.setting-field');
      if(row)row.hidden=name==='serial_5h_switch_percent'?mode!=='custom_threshold':mode!=='reserve_aware';
    }
    onSaved?.(false);
  }
  function readControl(spec,control) {
    if(spec[0]==='window_order')return control.value.split(',');
    if(spec[3]==='boolean')return control.checked;
    if(spec[3]==='number')return control.value===''?null:Number(control.value);
    if(spec[3]==='duration') {
      const unit=control.parentElement.querySelector('select').value;
      return control.value===''?null:control.value+unit;
    }
    return control.value.trim();
  }
  function writeControl(spec,control,value) {
    if(spec[3]==='boolean'){control.checked=!!value;return;}
    if(spec[3]==='duration') {
      const duration=String(value||'');
      const matches=[...duration.matchAll(/([\d.]+)(h|m|s)/g)];
      const seconds=matches.reduce((sum,m)=>sum+Number(m[1])*({h:3600,m:60,s:1}[m[2]]),0);
      const unit=seconds>0&&seconds%3600===0?'h':seconds>0&&seconds%60===0?'m':'s';
      control.value=seconds/({h:3600,m:60,s:1}[unit]);
      control.parentElement.querySelector('select').value=unit;return;
    }
    control.value=Array.isArray(value)?value.join(','):value??'';
  }
  function planRows() {
    const list=byId('account-plans');list.replaceChildren();
    const ids=[...new Set([...accounts,...Object.keys(draft.quota_account_plans||{})])].sort();
    for(const id of ids) {
      const row=make('div','plan-row'),label=make('label','',id),select=make('select');
      select.setAttribute('aria-label','套餐 '+id);
      select.append(new Option('自动识别（无法识别时用默认）',''));
      for(const [value,text]of plans)select.append(new Option(text,value));
      select.value=draft.quota_account_plans?.[id]||'';
      select.addEventListener('change',()=>{const map={...(draft.quota_account_plans||{})};if(select.value)map[id]=select.value;else delete map[id];draft.quota_account_plans=map;update();});
      label.append(select);row.append(label);list.append(row);
    }
    if(!ids.length)list.append(make('p','muted','读取到账号后可在此单独指定套餐。'));
  }
  function mount() {
    for(const group of ['allocation','warmup','advanced','connection','expert'])byId('fields-'+group).replaceChildren();
    controls.clear();
    for(const spec of fields) {
      const [name,label,group,type,help]=spec;
      if(!(name in draft))continue;
      const row=make('div','setting-field');row.dataset.field=name;
      const heading=make('label','setting-label',label);heading.htmlFor='setting-'+name;
      row.append(heading);
      if(type==='plans') {
        const plansBox=make('div','');plansBox.id='account-plans';row.append(plansBox);
      } else {
        const control=make(type==='select'?'select':'input');control.id='setting-'+name;control.name=name;
        if(type==='select')for(const [value,text]of spec[5])control.append(new Option(text,value));
        else control.type=type==='boolean'?'checkbox':type==='number'||type==='duration'?'number':type==='url'?'url':'text';
        if(type==='number'){control.min=spec[5];control.max=spec[6];control.step=spec[7];}
        if(type==='duration') {
          control.min=0;control.step='any';
          const wrapper=make('div','duration-input'),unit=make('select');unit.setAttribute('aria-label',label+'单位');
          for(const [value,text]of [['s','秒'],['m','分钟'],['h','小时']])unit.append(new Option(text,value));
          wrapper.append(control,unit);row.append(wrapper);
          unit.addEventListener('change',()=>{draft[name]=readControl(spec,control);update();});
        } else row.append(control);
        if(type!=='boolean')control.required=true;
        if(type==='text'||type==='url')control.maxLength=4096;
        control.addEventListener(type==='text'||type==='number'||type==='duration'||type==='url'?'input':'change',()=>{
          draft[name]=readControl(spec,control);row.querySelector('.field-error')?.remove();control.removeAttribute('aria-invalid');update();
        });
        controls.set(name,control);writeControl(spec,control,draft[name]);
      }
      if(help && help!=='serial'){const text=make('p','field-help',help);text.id='help-'+name;row.append(text);controls.get(name)?.setAttribute('aria-describedby',text.id);}
      byId('fields-'+group).append(row);
    }
    planRows();update();
  }
  async function load(force=false) {
    if(loaded&&!force)return;
    if(loading)return loading;
    const ticket=serial;
    // Rapid tab switches share one read: a late response must never remount a
    // newer draft. Reset invalidates old reads without blocking a new login.
    const pending=(async()=>{
      const [settings,config]=await Promise.all([api('/settings'),api('/config')]);
      if(ticket!==serial)return;
      raw=config;initial=structuredClone(settings.values);draft=structuredClone(initial);loaded=true;mount();
      byId('settings-unavailable').hidden=true;
    })();
    loading=pending;
    try {await pending;} finally {if(loading===pending)loading=null;}
  }
  function showErrors(errors) {
    for(const [name,message]of Object.entries(errors||{})) {
      const control=controls.get(name),row=control?.closest('.setting-field');
      if(row){control.setAttribute('aria-invalid','true');row.querySelector('.field-error')?.remove();row.append(make('p','field-error',message));}
    }
    const first=Object.keys(errors||{}).find(name=>controls.has(name));
    if(first){const group=fields.find(spec=>spec[0]===first)[2];onView(group==='connection'||group==='expert'?'advanced':group);controls.get(first).closest('details')?.setAttribute('open','');controls.get(first).focus();}
  }
  async function save(event) {
    event?.preventDefault();if(saving||!loaded)return;
    const changes=changed();if(!Object.keys(changes).length)return;
    for(const name of Object.keys(changes)) {
      const control=controls.get(name);
      if(control&&!control.checkValidity()){onView(fields.find(s=>s[0]===name)[2].replace(/connection|expert/,'advanced'));control.closest('details')?.setAttribute('open','');control.reportValidity();return;}
    }
    saving=true;const ticket=serial;update();
    try {
      const current=await api('/config');
      if(ticket!==serial)return;
      const conflicts=conflictingFields(raw,current,changes);
      if(conflicts.length)throw new Error('这些设置已被其他页面修改：'+conflicts.map(k=>fields.find(s=>s[0]===k)?.[1]||k).join('、')+'。请放弃当前修改并重新读取后再保存。');
      const known=Object.fromEntries(Object.entries(current).filter(([k])=>k in initial));
      const validated=await api('/settings/validate',{method:'POST',body:{config:known,changes}});
      if(ticket!==serial)return;
      let writeError=null;
      try {await api('/config',{method:'PATCH',body:validated.changes});} catch(error){writeError=error;}
      if(ticket!==serial)return;
      // A timed-out PATCH is never retried. Read back both the persisted config
      // and effective runtime before claiming success or allowing another save.
      for(let attempt=0;attempt<20;attempt++) {
        const [saved,effective]=await Promise.all([api('/config'),api('/settings')]);
        if(ticket!==serial)return;
        const names=Object.keys(validated.changes);
        const persisted=names.every(k=>equal(saved[k],validated.changes[k]));
        const applied=names.every(k=>equal(effective.values[k],validated.changes[k]));
        if(persisted&&applied){raw=saved;initial=structuredClone(effective.values);draft=structuredClone(initial);mount();notify('已保存到 CPA，设置已生效。','success');await onSaved?.(true);return;}
        if(writeError&&!persisted)throw new Error('保存结果未确认，请重新读取并核对设置；未自动重复提交。');
        await new Promise(resolve=>setTimeout(resolve,500));
      }
      throw new Error('CPA 已收到保存请求，但暂未确认运行配置生效。请重新读取核对，当前编辑内容仍保留。');
    } catch(error) {if(ticket===serial){showErrors(error.fields);notify(error.message);}}
    finally {if(ticket===serial){saving=false;update();}}
  }
  form.addEventListener('submit',save);
  byId('save-settings').addEventListener('click',save);
  byId('discard-settings').addEventListener('click',async()=>{if(saving)return;try{await load(true);notify('已重新读取 CPA 设置，未保存的修改已放弃。','success');}catch(e){notify(e.message);}});
  window.addEventListener('beforeunload',event=>{if(dirty()){event.preventDefault();event.returnValue='';}});
  return {load,dirty,
    setAccounts(values){const next=[...values].sort();if(equal(next,accounts))return;accounts=next;if(loaded)planRows();},
    reset(){serial++;initial={};draft={};raw={};loaded=false;saving=false;loading=null;accounts=[];form.inert=false;controls.clear();form.querySelectorAll('.settings-fields').forEach(n=>n.replaceChildren());byId('save-bar').hidden=true;},
    isSaving:()=>saving
  };
}
