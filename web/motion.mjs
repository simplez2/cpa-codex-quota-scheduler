// Motion decorates the existing controls; it never changes values or delays actions.
export function createPanelMotion(root=document) {
  const reduced=matchMedia('(prefers-reduced-motion: reduce)');
  const fine=matchMedia('(hover: hover) and (pointer: fine)');
  const running=new WeakMap(),seen=new WeakSet();
  const easing='cubic-bezier(.16,1,.3,1)';
  function animate(node,frames,options={}) {
    if(!node)return;
    running.get(node)?.cancel();
    if(reduced.matches)return;
    const animation=node.animate(frames,{duration:480,easing,...options});
    running.set(node,animation);
    animation.finished.then(()=>{if(running.get(node)===animation)running.delete(node);},()=>{});
  }
  const observer=new IntersectionObserver(entries=>{
    for(const entry of entries)if(entry.isIntersecting) {
      observer.unobserve(entry.target);
      animate(entry.target,[{opacity:0,transform:'translateY(14px)'},{opacity:1,transform:'translateY(0)'}],{delay:Number(entry.target.dataset.motionDelay||0)});
    }
  },{threshold:.08});
  function mount(scope=root) {
    const targets=scope.querySelectorAll('.pool-weekly-panel,.concurrency-panel,.overview,.accounts,.detail-panel,.settings-fields,.settings-details,.diagnostic-drawer');
    let index=0;
    for(const node of targets)if(!seen.has(node)) {
      seen.add(node);node.dataset.motionDelay=String((index++%3)*55);observer.observe(node);
      if(node.matches('.pool-weekly-panel,.concurrency-panel,.overview,.detail-panel'))node.dataset.glassSurface='';
    }
  }
  const nav=root.querySelector('.view-nav');
  const lens=document.createElement('span');lens.className='nav-lens';lens.setAttribute('aria-hidden','true');
  nav.append(lens);nav.dataset.motionReady='';
  let indicator=null;
  function navigation() {
    const active=nav.querySelector('[aria-pressed=true]');if(!active||!active.offsetWidth)return;
    const to={transform:`translateX(${active.offsetLeft}px)`,width:`${active.offsetWidth}px`};
    if(lens.style.transform===to.transform && lens.style.width===to.width)return;
    const previous=indicator?.effect.getComputedTiming().progress;
    // Capture the painted position before cancel, so rapid reversals remain continuous.
    const from={transform:getComputedStyle(lens).transform,width:getComputedStyle(lens).width};
    indicator?.cancel();
    Object.assign(lens.style,to);
    if(!reduced.matches && previous!==undefined)indicator=lens.animate([from,to],{duration:420,easing});
    else indicator=lens.animate([to,to],{duration:0});
  }
  new ResizeObserver(navigation).observe(nav);
  let rendered=false,values=new Map();
  const selectors='#pool-weekly-percent,#pool-5h-percent,#concurrency-active,#concurrency-waiting,#concurrency-free,#eligible-count,#switch-count,#cooldown-count';
  function metrics() {
    for(const node of root.querySelectorAll(selectors)) {
      const text=node.textContent;
      if(values.has(node.id)&&values.get(node.id)!==text)animate(node,[{opacity:.25,transform:'translateY(5px)'},{opacity:1,transform:'translateY(0)'}],{duration:350});
      values.set(node.id,text);
    }
  }
  function renderedState() {
    mount();metrics();navigation();
    if(!rendered){rendered=true;animate(root.querySelector('.view-nav'),[{opacity:0,transform:'translateY(8px)'},{opacity:1,transform:'translateY(0)'}]);}
  }
  function view(panel) {
    navigation();mount(panel);
    animate(panel,[{opacity:0,transform:'translateY(10px)'},{opacity:1,transform:'translateY(0)'}],{duration:380});
  }
  function feedback(node) { if(!node.hidden)animate(node,[{opacity:0,transform:'translateY(-5px)'},{opacity:1,transform:'translateY(0)'}],{duration:280}); }
  function rows(body,previous) {
    for(const row of body.children) {
      const before=previous.get(row.dataset.authId);
      const bars=row.querySelectorAll('.bar-fill');
      if(before)bars.forEach((bar,index)=>{
        if(before[index]!==undefined&&before[index]!==bar.style.width)animate(bar,[{width:before[index]},{width:bar.style.width}],{duration:600});
      });
      else if(previous.size)animate(row,[{opacity:.2},{opacity:1}],{duration:280});
    }
  }
  root.addEventListener('toggle',event=>{
    if(event.target.tagName==='DETAILS'&&event.target.open)for(const node of [...event.target.children].slice(1))animate(node,[{opacity:0,transform:'translateY(-5px)'},{opacity:1,transform:'translateY(0)'}],{duration:280});
  },true);
  root.addEventListener('click',event=>{
    const button=event.target.closest('button');
    if(button&&!button.disabled&&!button.closest('.view-nav'))animate(button,[{scale:'.97'},{scale:'1'}],{duration:300});
  });
  let pointerFrame=0,pointerTarget=null,pointerLens=null,pointerX=0,pointerY=0;
  root.addEventListener('pointermove',event=>{
    if(reduced.matches||!fine.matches)return;
    const target=event.target.closest('[data-glass-surface]');
    if(pointerTarget&&pointerTarget!==target)pointerTarget.removeAttribute('data-pointer');
    pointerTarget=target;
    const lens=event.target.closest('.quota-lens');
    if(pointerLens&&pointerLens!==lens)pointerLens.style.removeProperty('translate');
    pointerLens=lens;
    if(!target)return;
    pointerX=event.clientX;pointerY=event.clientY;
    if(!pointerFrame)pointerFrame=requestAnimationFrame(()=>{
      pointerFrame=0;if(!pointerTarget)return;
      const rect=pointerTarget.getBoundingClientRect();
      pointerTarget.dataset.pointer='';pointerTarget.style.setProperty('--pointer-x',`${pointerX-rect.left}px`);pointerTarget.style.setProperty('--pointer-y',`${pointerY-rect.top}px`);
      if(pointerLens){const box=pointerLens.getBoundingClientRect();pointerLens.style.translate=`${Math.max(-4,Math.min(4,(pointerX-box.left-box.width/2)/20))}px ${Math.max(-4,Math.min(4,(pointerY-box.top-box.height/2)/20))}px`;}
    });
  },{passive:true});
  root.addEventListener('pointerout',event=>{if(pointerTarget&&!pointerTarget.contains(event.relatedTarget)){pointerTarget.removeAttribute('data-pointer');pointerTarget=null;}if(pointerLens&&!pointerLens.contains(event.relatedTarget)){pointerLens.style.removeProperty('translate');pointerLens=null;}},{passive:true});
  reduced.addEventListener('change',()=>{for(const animation of document.getAnimations())animation.cancel();navigation();});
  // A neutral center with displacement concentrated on the rounded rim.
  // R/G encode x/y vectors, as described in kube.io's CSS/SVG refraction article.
  const canvas=document.createElement('canvas');canvas.width=256;canvas.height=96;
  const context=canvas.getContext('2d');
  if(context) {
    const pixels=context.createImageData(256,96);
    for(let y=0;y<96;y++)for(let x=0;x<256;x++) {
      const qx=Math.abs(x-127.5)-108,qy=Math.abs(y-47.5)-28;
      const dx=Math.max(qx,0),dy=Math.max(qy,0);
      const distance=Math.hypot(dx,dy)+Math.min(Math.max(qx,qy),0)-19;
      const strength=distance<=0?Math.max(0,1-Math.abs(distance+5)/11):0;
      const length=Math.hypot(dx,dy)||1;
      let nx=dx/length*Math.sign(x-127.5),ny=dy/length*Math.sign(y-47.5);
      if(dx===0&&dy===0){if(qx>qy)nx=Math.sign(x-127.5);else ny=Math.sign(y-47.5);}
      const offset=(y*256+x)*4;
      pixels.data[offset]=128+nx*strength*100;pixels.data[offset+1]=128+ny*strength*100;
      pixels.data[offset+2]=128;pixels.data[offset+3]=255;
    }
    context.putImageData(pixels,0,0);
    root.getElementById('glass-map').setAttribute('href',canvas.toDataURL('image/png'));
    // Chromium implements SVG backdrop filters; all other engines keep the blur fallback.
    document.documentElement.dataset.refraction=String(/Chrome|Chromium|Edg\//.test(navigator.userAgent));
  }
  return {rendered:renderedState,view,feedback,rows,dialog:node=>animate(node,[{opacity:0,transform:'translateY(12px) scale(.97)'},{opacity:1,transform:'translateY(0) scale(1)'}],{duration:340})};
}
