const {test}=require('node:test'),assert=require('node:assert/strict'),fs=require('node:fs'),vm=require('node:vm'),path=require('node:path');
const html=fs.readFileSync(path.join(__dirname,'../templates/production_render_create.html'),'utf8');
const script=[...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m=>m[1]).find(s=>s.includes('function livestreamTalkGroups'));
function fixture({batch=false,include=true}={}){
 class Element{
  constructor(){this.children=[];this.style={};this.disabled=false;this.checked=false;this.open=false;this.hidden=false;this.indeterminate=false;this.value='';this.listeners={}}
  append(...nodes){this.children.push(...nodes)} prepend(...nodes){this.children.unshift(...nodes)} replaceChildren(){this.children=[]}
  setAttribute(k,v){this[k]=v} addEventListener(k,fn){this.listeners[k]=fn} showModal(){this.open=true} close(){this.open=false;this.listeners.close?.()} focus(){}
  querySelector(){return new Element()}
  querySelectorAll(selector){const all=this.children.flatMap(x=>x instanceof Element?[x,...x.querySelectorAll('*')]:[]);return selector==='*'?all:all.filter(n=>n.name==='talk_ids')}
 }
 const talk=(id,day,stage,ready=true)=>({talk_id:id,title:id,starts_at:`2026-11-0${day}T10:00:00+09:00`,dayLabel:'Day '+(day-4),dateLabel:'Nov '+day,stageLabel:stage,ready,timeLabel:'10:00',reason:ready?'':'No cuts'});
 const talks=[talk('a',5,'Main'),talk('b',5,'Main'),talk('uncut',5,'Main',false),talk('c',5,'Talks'),talk('d',6,'Main'),{...talk('restricted',5,'Main'),manualOnly:true}];
 const ctx={appendProductionRecordingTag:()=>{},populateRenderSettings:()=>{},createRecordingControls:()=>({refresh(){}}),Map,Set,URLSearchParams,document:{createElement:()=>new Element(),createTextNode:x=>x},fetch:async()=>({ok:true,headers:{get:()=> 'application/json'},json:async()=>({talks})})};
 vm.createContext(ctx);vm.runInContext(script,ctx);
 function mount(isBatch,includes){
  const elements=new Map(),get=id=>{if(!elements.has(id))elements.set(id,new Element());return elements.get(id)},root=new Element(),trigger=new Element();
  root.querySelector=selector=>{const role=selector.match(/data-role="([^"]+)"/)[1];return role==='include-talks'&&isBatch?null:get(role)};
  get('form').elements={template_id:{value:'-1'},conference:{value:'seoul'},output:{value:isBatch?'separate':'one'}};
  get('include-talks').checked=includes;get('talk-options').hidden=!isBatch&&!includes;
  ctx.createRenderCreationDialog(root,trigger,{batch:isBatch});
  return {get,root,trigger,checks:()=>get('talks').querySelectorAll('*'),open:async()=>trigger.onclick()};
 }
 return {...mount(batch,include),ctx,mount};
}

test('talks are grouped by day and stage, with whole-group and individual selection',async()=>{
 const f=fixture();await f.open();const checks=f.checks();const group=checks.find(c=>c['aria-label']==='Select Day 1 · Main'),a=checks.find(c=>c.value==='a'),b=checks.find(c=>c.value==='b'),uncut=checks.find(c=>c.value==='uncut');
 assert.equal(f.get('create').disabled,true);group.checked=true;group.onchange();assert.equal(a.checked,true);assert.equal(b.checked,true);assert.equal(uncut.checked,false);assert.equal(group.indeterminate,false);assert.equal(checks.find(c=>c.value==='restricted').checked,false);assert.match(f.get('create').textContent,/2 talks/);
 a.checked=false;a.onchange();assert.equal(group.indeterminate,true);assert.equal(group.checked,false);
 const combo=checks.find(c=>c['aria-label']==='Select Day 1 · Talks');combo.checked=true;combo.onchange();assert.equal(checks.find(c=>c.value==='c').checked,true);assert.equal(checks.find(c=>c.value==='d').checked,false);assert.match(f.get('create').textContent,/2 talks/);
 assert.equal(checks.filter(c=>c['aria-label']?.startsWith('Select ')).length,3);
 assert.equal(checks.some(c=>c['aria-label']==='Select Day 1 · Nov 5'),false);
 assert.equal(f.get('talks').children.length,3);
 f.get('clear').onclick();assert.equal(f.get('create').disabled,true);
 f.get('select-all').onclick();assert.match(f.get('create').textContent,/4 talks/);
 const restricted=checks.find(c=>c.value==='restricted');assert.equal(restricted.disabled,false);assert.equal(restricted.checked,false);restricted.checked=true;restricted.onchange();assert.match(f.get('create').textContent,/5 talks/);
});
test('dialogs have no day/stage or output dropdowns',()=>{
 assert.doesNotMatch(html,/<select[^>]*(?:name="(?:day|stage|output)"|id="render-create-output")/);
});

test('New render can start blank while Batch loads its own independent talk selection',async()=>{
 const single=fixture({include:false});await single.open();assert.equal(single.get('create').disabled,false);assert.equal(single.get('create').textContent,'Create blank render');assert.equal(single.get('talk-options').hidden,true);
 const batch=single.mount(true,false);await batch.open();assert.equal(batch.get('create').disabled,true);
 batch.get('select-all').onclick();assert.equal(batch.get('create').textContent,'Create 4 renders');assert.equal(batch.checks().find(c=>c.value==='restricted').checked,false);
 assert.equal(single.get('create').textContent,'Create blank render');assert.equal(single.checks().length,0);
 single.get('include-talks').checked=true;await single.get('include-talks').onchange();assert.equal(single.get('create').disabled,true);assert.equal(single.checks().filter(c=>c.checked).length,0);
 assert.equal(batch.checks().filter(c=>c.name==='talk_ids'&&c.checked).length,4);
});

test('turning Include talks off while data loads keeps blank creation available',async()=>{
 const f=fixture({include:false});await f.open();let respond;f.ctx.fetch=()=>new Promise(resolve=>respond=resolve);
 f.get('include-talks').checked=true;const work=f.get('include-talks').onchange();
 f.get('include-talks').checked=false;f.get('include-talks').onchange();assert.equal(f.get('create').disabled,false);
 respond({ok:true,headers:{get:()=> 'application/json'},json:async()=>({talks:[]})});await work;
 assert.equal(f.get('create').disabled,false);assert.equal(f.get('create').textContent,'Create blank render');assert.equal(f.get('status').textContent,'');
});
